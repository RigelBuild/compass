# Compass Manager comms substrate — roster query, coordination channel, pinned board

Status: Draft
Tracker: SEA-1721, SEA-1722, SEA-1723

> DRAFT — all nine Open Questions ratified by Matt 2026-07-31 (see Decisions
> section); freezes at merge.
> Cards: SEA-1721 (agent roster query), SEA-1722 (manager-owned coordination
> channel), SEA-1723 (pinned board). One record because the three primitives
> compose: the roster and the coordination channel both derive from the agent
> tree (DL-095), and the pinned board's first two homes are the root
> #announcements-class channel and the coordination channel this record mints.

## Problem / Intent

The Compass Manager operating model — a root Manager coordinating mid-level
Managers, each coordinating its own reports — needs three comms primitives that
do not exist today:

1. **No roster pull (SEA-1721).** Presence is an *event only*:
   `AgentPresenceChanged { agent_account_id, presence }`
   (`compass/proto/compass/v1/comms.proto:498-503`) rides `SubscribeComms`, and
   no query RPC exists — the `CommsService` block (`comms.proto:34-110`)
   contains no `ListPresence`/`GetRoster`; `ListAccounts` (`comms.proto:45`)
   returns accounts, not presence. An agent starting cold (or resuming after a
   drop) cannot ask "who is in my tree, who is online, what is each doing."
   The event also carries **no activity string** — the enum is bare 4-state
   (`AgentPresence`, `comms.proto:507-513`, per DL-074).
2. **No restricted-post / mandatory-subscribe channel (SEA-1722).** A mid-level
   Manager needs a coordination channel for its reports — built on the same
   missing primitive as top-level #announcements/#incidents: a post ACL
   (owner-only where wanted) plus every report subscribed and unable to
   unsubscribe (the coordination channel itself is `OWNER_ONLY` — resolved,
   Matt 2026-07-31, OQ-9). Today `Channel`
   (`comms.proto:204-221`) has membership + a free subscribe opt-in
   (`member_account_ids = 5`, `subscriber_account_ids = 6`) and no post ACL of
   any kind; `UpdateChannelMembers` "covers join, subscribe-toggle,
   DM-expansion, and share-replacement" (`comms.proto:62-65`) with symmetric
   `subscribe_account_ids`/`unsubscribe_account_ids` arms
   (`comms.proto:601-612`) — nothing prevents a member unsubscribing, and any
   member may post. `ChannelGroupVisibility` (OWNER/SHARED,
   `comms.proto:196-201`) is a *visibility* axis and must not be overloaded to
   mean post permission.
3. **No pinned board (SEA-1723).** There is no server-side "editable headline
   every Manager sees on startup, re-pushed on edit" (e.g. "CI is red — see
   Thread 12344 in #incidents"). This is explicitly NOT DL-096's sidebar pins,
   which are a per-user, client-local, `localStorage`-backed *presentation*
   preference over the agent set ("the pin set is a per-user client-local UI
   preference (`localStorage`-backed …)", `DECISIONS.md:156`; "Pins are
   presentation over the tree, never structure",
   `compass-sidebar-pins/design.md:74`) — no server message, no broadcast, no
   redelivery. The pinned board here is server-side broadcast *content*.

This record designs the three together, riding the frozen notification-delivery
rail (DL-071/072/073) and the agent tree (DL-095), design only — implementation
lands later in the `RigelBuild/compass` repo.

## Approach

Three primitives, one composition rule: **everything derives from the agent
tree and rides the existing rails.** The roster reads the tree
(`AgentAccount.parent_agent_id`, "empty = root … editable via ReparentAgent …
The server validates same-owner and no-cycle on every write",
`comms.proto:158-162`) plus the D4 in-memory presence snapshot. The
coordination channel is auto-provisioned *from* tree edges and its membership
tracks them. The pinned board is channel content delivered over DL-071/072/073
— no parallel delivery rail, no new authz model (everything executes under
`WithActor(agent account)` with the existing D9 checks, exactly as
`compass-agent-comms-tools/design.md:182-197` fixed: "No new authz code is
written; no new authz policy is invented").

### A1 — SEA-1721: roster query — a `GetRoster` RPC + `compass_roster` tool over the D4 presence snapshot

**Read path, not a new projection.** DL-074 froze presence as a 4-state
in-memory projection: "the hub keeps the last-published state per agent in
memory (presence is ephemeral truth about a live pipeline; it deliberately has
NO durable table)" (`compass-notification-delivery/design.md:491-494`), with
restart reconciliation via `GetAgentStatus` at Runner re-enroll (`:495-503`).
The roster RPC is a *snapshot read* of that hub state joined with the agent
tree from the store AND the durable activity table (below) — no heartbeat, no
second presence source; the only new table is `agent_activity` (the string,
never the enum).

**Two legs, the ratified tool shape.** (1) A public `CommsService.GetRoster`
RPC (UI + human callers, scoped by the caller's D9 visibility). (2) A native
agent tool `compass_roster`, registered at agent boot in `createCommsTools`
(`compass-agent-comms-tools/design.md:484-485`), carried as a new arm on the
shared `CommsCallRequest`/`CommsCallResult` envelopes
(`agent_gateway.proto:77-96` — the oneof today holds `post`/`list`) and relayed
via `RelayCommsCall` (`runner.proto:428-440`), session-resolved identity as
always.

**Scope: tree-neighborhood default, owner-wide by filter.** The default answer
set is the calling agent's tree neighborhood — parent, siblings (peers), and
direct children — with a `scope` enum widening to full subtree or owner-wide
(all agents sharing `AgentAccount.owner_user_id`). See OQ-2 (resolved).

**The activity string is NEW state with no source today — agent-set AND
durable.** `AgentPresenceChanged` carries only `{agent_account_id, presence}`
(`comms.proto:498-503`) and the enum is bare 4-state (`comms.proto:507-513`).
Source (resolved, Matt 2026-07-31): **agent-set self-report** — a
`compass_set_status` arm on the same comms-call family, mirroring the
Cotal-status precedent DL-074's vocabulary was aligned to. Server-derived
activity (inferring from the current turn's message) is rejected: it guesses,
and the agent knows. Unlike the presence enum, the STRING is durable: it is
written through to its own store table (`agent_activity`, T2) and published
as additive `AgentPresenceChanged.activity = 3` for live streams. This is a
deliberate, Matt-ratified divergence from DL-074's in-memory posture FOR THE
STRING ONLY — the 4-state presence enum stays in-memory per DL-074, but a
Server restart recovers every agent's activity string from Postgres; the
agent-side re-publish on session (re)attach is kept as a FRESHNESS mechanism,
not the recovery path. See OQ-1 (resolved).

### A2 — SEA-1722: coordination channel — a channel-level `post_policy` + `mandatory_subscription`, auto-provisioned from tree edges

**Model it on `Channel`, as policy fields — not a new `ChannelKind`, not
visibility.** Two additive fields on `Channel` (`comms.proto:204-221`):

- `post_policy` (enum `ChannelPostPolicy`: `OPEN = 0` — today's behavior, the
  zero value; `OWNER_ONLY = 1` — only `owner_account_id` may post; the server
  rejects any other `PostMessage`/`comms_post_message` with the same in-band
  error a non-member gets).
- `owner_account_id` — the owning account (the Manager agent for a
  coordination channel; a human/root account for #announcements-class
  channels). Server-set at provision; channels without a post policy leave it
  empty.
- `mandatory_subscription` (bool) — every member is force-subscribed:
  `UpdateChannelMembers`' `unsubscribe_account_ids` arm
  (`comms.proto:610-611`) is rejected for such a channel, and D1's subscriber
  resolution gains a third disjunct beside the home-channel repair
  (`compass-notification-delivery/design.md:117-120`: `cm.subscribed OR
  cm.channel_id = aa.home_channel_id` → `… OR ch.mandatory_subscription`), so
  the guarantee is enforced read-side exactly like the home channel, immune to
  row-flag drift.

Why not a new `ChannelKind`: kind encodes *conversation topology*
(CHANNEL/DM/GROUP_DM, `comms.proto:226-230` — DMs widen to GROUP_DM), and
post-policy is orthogonal to topology; a kind would force a matrix. Why not a
per-member locked-subscribe bit: the policy is channel-level ("same policy
as #announcements" — *all* reports, always), and a per-member bit invites drift
the read-side disjunct then has to paper over. Why not visibility:
`ChannelGroupVisibility` (`comms.proto:196-201`) answers *who can see*, never
*who can post* — overloading it is explicitly ruled out. See OQ-3 (resolved).

**Auto-provision hangs off the STORE-level parent-edge writers — complete by
construction.** `agent_accounts.parent_agent_id` has exactly two store-level
writers: `store.CreateAgent` (`go/internal/store/accounts.go:131`), whose
INSERT writes the column (`"INSERT INTO agent_accounts (account_id,
owner_user_id, home_channel_id, persona, parent_agent_id) VALUES ($1, $2, $3,
$4, NULLIF($5, ''))"`, `accounts.go:156-158`), and `store.ReparentAgent`
(`accounts.go:280`), whose UPDATE rewrites it (`"UPDATE agent_accounts SET
parent_agent_id = NULLIF($2, '') WHERE account_id = $1"`,
`accounts.go:346-348`). Every RPC-level parent-edge path funnels through those
two: the spawn chain ("executes the full create chain server-side",
`compass-agent-spawn-despawn/design.md:201-205`) and the public `CreateAgent`
RPC (handler `Comms.CreateAgent`, `comms.go:101-136`, calling
`c.store.CreateAgent` at `comms.go:126` with the request's
`parent_agent_id` — `comms.proto:534-537`; named as a parent-assignment site
by `compass-agent-trees/design.md:78-86`) both end in `store.CreateAgent`;
reparent ends in `store.ReparentAgent`. Those two store methods invoke the
reconciler through a **store-registered in-transaction hook** — a
`func(ctx, tx, managerID store.AccountID) error` the comms layer registers at
wiring time — so the reconcile runs on the store's own `*tx`, in the SAME
transaction as the parent-edge write, WITHOUT the store package importing
comms/channel types (the store owns the transaction; the hook is an injected
callback, never a `*Comms` method reached from inside `*Store`). It covers
spawn, public `CreateAgent`, `ReparentAgent`, and any future writer (bulk
import, restore, migration tooling) by construction, not by RPC enumeration.
The hook does the in-tx work — channel upsert, membership rows, D2 cursor
seeds — all on the same `tx`; the `ChannelChanged` event is emitted
**post-commit, best-effort** by the comms caller (never inside the tx),
self-healing on the next reconcile / D1 sweep if the emit is lost, so a
dropped event never leaves the tree edge and the channel state divergent. A
greppable invariant comment lands at the `parent_agent_id` column definition
and on both store methods: `INVARIANT: every write of
agent_accounts.parent_agent_id must invoke the coordination-channel hook`.
Trigger: when an agent M gains its FIRST report, the server provisions M's coordination channel
(`owner_account_id=M`, `mandatory_subscription=true`,
`post_policy=OWNER_ONLY` — resolved, Matt 2026-07-31, see the post-policy
paragraph below and OQ-9; members = M + its reports) in M's
owner's namespace; subsequent parent-edge writes reconcile membership (report
added on create-with-parent/reparent-in, removed on reparent-out — the
departing member gets the final `ChannelChanged` via `removed_account_ids`,
`comms.proto:461-472`). Despawn does NOT dissolve membership: per DL-077 "the
agent account persists (teardown is compute-only)" (`DECISIONS.md:134`), so
the channel and its roster survive respawn. The channel is never deleted on
losing the last report — it goes dormant (channel deletion doesn't exist as
an RPC today, and history must survive). See OQ-4 (trigger + lifecycle) and
OQ-9 (post policy), both resolved (Matt 2026-07-31).

**Post policy: OWNER_ONLY — the channel is a one-way directive surface;
coordination flows through DMs (resolved, Matt 2026-07-31).** The
coordination channel is provisioned `post_policy=OWNER_ONLY` +
`mandatory_subscription=true`: only the Manager posts, and every report is
force-subscribed — a broadcast directive surface, never a discussion floor.
Report→manager and lateral (report↔report) coordination flows through direct
DMs and small targeted group DMs — the existing `ChannelKind` topology:
`CHANNEL_KIND_DM = 1` / `CHANNEL_KIND_GROUP_DM = 2` (`comms.proto:226-230`),
where "a DM is a direct conversation (human↔human, human↔agent, or
agent↔agent) that widens into a GROUP_DM as members are added"
(`comms.proto:223-225`) — so a report replying to a directive, raising an
Ask, or coordinating with two peers on one issue opens a DM/group DM with
exactly the relevant parties. STANDING DIRECTIVE (Matt): agents at EVERY
level heavily prefer direct DMs / small targeted group DMs for coordination,
to keep coordination-token-cost low — the coordination channel carries only
broadcast directives every report must see, never per-report chatter. That
token-cost argument is the design rationale for OWNER_ONLY: the vast
majority of manager↔report coordination need not be seen by every other
report, and an OPEN channel would burn every subscribed report's tokens
broadcasting it.

**Relationship to SEA-1622 — precursor, not part-of.** SEA-1622 (unify
channels + workspaces under the agent tree) is post-MVP and depends on
agent-trees: "Unifying them under the agent tree … is SEA-1622, post-MVP …
the two trees coexist until SEA-1622 lands"
(`compass-agent-trees/design.md:230-236`; DL-095, `DECISIONS.md:43`). This
record builds the *ACL + mandatory-subscription mechanism* SEA-1622 will later
compose onto tree-derived channel scoping. The fields live on `Channel`, not
on the `ChannelGroup` namespace tree, precisely so the later folding moves the
channel's *location* without touching its *policy*.

### A3 — SEA-1723: pinned board — a pure pointer set over existing topic-scoped messages; edit = topic-mandatory post + repoint, so redelivery IS delivery

**Not DL-096.** Stated once more for the freeze: DL-096's sidebar pins are
"a per-user client-local UI preference (`localStorage`-backed …)"
(`DECISIONS.md:156`) — presentation, no server state. SEA-1723's pinned board
is server-side channel content: authored by the channel owner, stored in
Postgres, broadcast to every subscriber, redelivered on edit. The two share a
word, nothing else; neither supersedes the other.

**The board is a pure pointer set; pinning never creates a message.** A
pinned-board entry references an EXISTING `Message` row by id — and under the
frozen topic model every message lives in exactly one topic ("every message
belongs to exactly one topic (`messages.topic_id NOT NULL`) and stores only
that topic id — never a channel id", DL-098, `DECISIONS.md:170`;
`Message.topic_id = 2`, `comms.proto:276-284`), so a pin points at a message
in one of the channel's topics, reached through `topics.channel_id`. Pinning
is orthogonal to posting: it mints nothing, so DL-099's single-write-path
invariant ("a comms `Message` is created only by an explicit
`comms_post_message(topic)` call (agents) or the human client's PostMessage",
`DECISIONS.md:171`) is not even engaged. "Editing the headline" is two
composed ops over existing paths: post a NEW message via the normal
topic-mandatory path (`compass_post_message`/`PostMessage`, whose request
carries the mandatory `oneof topic { topic_id = 6; topic_name = 7; }`,
`comms.proto:679-682`), then repoint the pin at the new id (compare-and-swap,
T6). This one choice makes every hard delivery question dissolve into the
frozen rail:

- **Redeliver-on-edit = plain delivery of a new message.** DL-072's
  exactly-once comes from agent-side `message_id` dedup
  (`compass-notification-delivery/design.md:360-365`); re-pushing an *edited*
  message under its old id would be swallowed by that dedup within a session.
  A new id is a new delivery — dedup, cursor math (`agent_delivery_cursors`,
  `design.md:264-283`), the turn-settle gate (DL-071), and the reconnect sweep
  all apply unchanged. Zero special cases in the rail.
- **Startup delivery = a pin sweep beside the cursor sweep.** At
  `StartAgentSession` the D2 sweep already redelivers unacked messages
  (`design.md:340-346`). A sibling step dispatches each subscribed channel's
  CURRENT pinned message(s) as ordinary `DeliverControl` ops (full
  `compass.v1.Message` per DL-073, `DECISIONS.md:130`) — *regardless of
  cursor position*, because a pin may be far below `acked_seq` (the agent
  acked it long ago; a fresh session must still see the standing headline).
  Per-session `message_id` dedup makes the pin sweep idempotent within a
  session; across sessions the re-push is the point.
- **Edit lands at the turn-settle edge, never as a steer.** A board edit is a
  ping-class deliver under DL-071's timing (human-authored at post,
  agent-authored at the author's settle). A genuinely interruptive emergency
  already has a mechanism — an `@`-mention steer — and the board must not
  become a second interrupt path. See OQ-5 (resolved).

**Authorship = the channel's post policy.** On a `post_policy=OWNER_ONLY`
channel only the owner pins (root on #announcements-class channels; the
mid-level Manager on its coordination channel). On an OPEN channel, pinning
follows posting (any member) for MVP — the board primitive is policy-agnostic;
the ACL does the restricting.

**Shape: a small ordered set per channel, server-capped.** One slot forces
destructive overwrites of unrelated headlines ("CI red" vs "deploy freeze");
an unbounded set becomes a second notifications surface and collides with
DL-054. A per-channel cap (5, resolved OQ-5) keeps it a *board*, not a feed.
See OQ-5/OQ-6 (both resolved).

**DL-054 positioning.** DL-054: "Notifications v1 is chat pings plus asks …
no notifications page, centre, badge, or read state" (`DECISIONS.md:127`). The
pinned board introduces none of those: it is channel-scoped broadcast content
delivered as chat (a `DeliverControl` message), with no read state (the
delivery cursor is delivery bookkeeping, not user-facing read state), no
badge, no centre. Judged compatible; confirmed by Matt (OQ-6 resolved, no
DL-054 amendment).

## Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Two-repo split.** This record lives in `sealed` (design corpus);
  implementation lands in `RigelBuild/compass`. All file:line citations
  below are into the compass clone.
- **Additive-only proto changes.** New fields, new enum values, new RPCs, new
  oneof arms only — no renumbering, no wire-type changes, buf-breaking-safe.
- **SEA-1267 gen fence.** Presence/roster surfaces are PUBLIC, not gen-fenced
  (matching `AgentPresenceChanged`'s "PUBLIC … NOT gen-fenced" posture,
  `comms.proto:496-497`); the `CommsCallRequest`/`RelayCommsCall` carrier
  family keeps its existing fence classification. compass-repo is the sole
  proto-tree writer (single buf.gen).
- **Postgres is the store of record** for channel policy, pins, AND the
  activity string: the presence 4-state enum stays in-memory per DL-074 (no
  durable presence table), but the agent-set activity string is durable in
  its own `agent_activity` table — a deliberate divergence ratified by Matt
  (2026-07-31), so a Server restart recovers it from Postgres.
- **Single-Server-process assumption.** The presence ENUM — and therefore
  the roster's presence join — is in-memory single-Server-process state per
  DL-074; `GetRoster`'s correctness leans on the in-process hub snapshot for
  the enum. The activity string is NOT single-process-dependent (it lives in
  the DB). Horizontal Server scale-out is out of scope and would revisit
  this record.
- **Delivery rides DL-071/072/073 unchanged**: turn-settle fan-out, durable
  `(agent_account_id, channel_id)` cursor, `DeliverControl` = full
  `compass.v1.Message`, ack = `delivery_ack{message_id}`. No parallel rail, no
  new control-op class.
- **Authz: session-resolved `WithActor`, existing D9 checks** — no new authz
  code, no new policy class beyond the two channel fields this record adds.
- **Owner lanes**: compass-repo (proto text), compass-comms (Server
  store/delivery/RPC), compass-agent (TS tool leg), compass-ui (render).

## Plan

### T1 — proto delta (gates all others) — [compass-repo] [SEA-1721 + SEA-1722 + SEA-1723]

One additive change-set to `proto/compass/v1/`:

- `comms.proto` — `Channel` gains `ChannelPostPolicy post_policy = 7;`,
  `string owner_account_id = 8;`, `bool mandatory_subscription = 9;` (next
  free fields after `subscriber_account_ids = 6`, `comms.proto:220`); new
  `enum ChannelPostPolicy { CHANNEL_POST_POLICY_OPEN = 0;
  CHANNEL_POST_POLICY_OWNER_ONLY = 1; }`.
- `comms.proto` — `rpc SetChannelPolicy(SetChannelPolicyRequest) returns
  (SetChannelPolicyResponse)` — owner/operator create-or-update of the three
  policy fields on an existing channel: `SetChannelPolicyRequest {
  string channel_id = 1; ChannelPostPolicy post_policy = 2;
  string owner_account_id = 3; bool mandatory_subscription = 4; }`,
  response `{ Channel channel = 1; }`. See T4 for the cursor-seeding txn
  contract (a mandatory flip must not mint un-seeded delivery targets).
- `comms.proto` — `rpc GetRoster(GetRosterRequest) returns (GetRosterResponse)`
  in the `CommsService` block (`comms.proto:34-110`);
  `GetRosterRequest { RosterScope scope = 1; string agent_account_id = 2; }`
  (agent id optional for human/UI callers naming a vantage point; agents get
  it session-resolved), `enum RosterScope { ROSTER_SCOPE_NEIGHBORHOOD = 0;
  ROSTER_SCOPE_SUBTREE = 1; ROSTER_SCOPE_OWNER = 2; }`,
  `GetRosterResponse { repeated RosterEntry entries = 1; }`,
  `RosterEntry { string agent_account_id = 1; string handle = 2;
  string display_name = 3; string parent_agent_id = 4;
  AgentPresence presence = 5; string activity = 6; int64
  activity_at_unix_ms = 7; }`.
- `comms.proto` — `AgentPresenceChanged` gains `string activity = 3;`
  (additive beside `presence = 2`, `comms.proto:498-503`) so live streams and
  the roster read one vocabulary.
- `comms.proto` — pinned board: `message PinnedEntry { string message_id = 1;
  int32 position = 2; int64 pinned_at_unix_ms = 3; string
  pinned_by_account_id = 4; }`; `Channel` gains
  `repeated PinnedEntry pinned_entries = 10;`; new
  `rpc UpdatePinnedBoard(UpdatePinnedBoardRequest) returns
  (UpdatePinnedBoardResponse)` — request `{ string channel_id = 1;
  oneof op { PinMessage pin = 2; string unpin_message_id = 3; } }` where
  `PinMessage { string message_id = 1; string replace_message_id = 2; }`
  pins an EXISTING message (`message_id` must already live in a topic of
  this channel) and optionally repoints (`replace_message_id` names the
  currently pinned entry it replaces — compare-and-swap, T6). No blocks, no
  message minting anywhere in this RPC. Response `{ Channel channel = 1; }`
  (the updated channel carrying `pinned_entries`). Board changes ride the
  existing `ChannelChanged` event (`comms.proto:461-472`) since the board is
  `Channel` state.
- `agent_gateway.proto` — `CommsCallRequest.call` oneof
  (`agent_gateway.proto:77-84`) gains `GetRosterRequest roster = N;`,
  `SetAgentStatusRequest set_status = N+1;`, `UpdatePinnedBoardRequest
  pin = N+2;`; `CommsCallResult.result` (`agent_gateway.proto:89-96`) gains
  the matching response arms. `SetAgentStatusRequest { string activity = 1; }`
  / `SetAgentStatusResponse {}` (comms.proto; agent-only via the relay — no
  public RPC needed for MVP).

Interfaces: the proto messages/RPCs above, verbatim; `buf lint` +
`buf breaking` clean; regenerated Go/TS in the single buf.gen lane.

### T2 — roster read path: hub snapshot join + durable activity store + `GetRoster` handler — [compass-comms] SEA-1721

The hub's in-memory presence map (DL-074) stays ENUM-only; the activity
string lives in a new durable store table; the handler joins hub + tree +
activity table.

Migration — the string's store of record, keyed by the agent
(`agent_accounts.account_id` is `TEXT PRIMARY KEY REFERENCES accounts (id)`,
`migrations/0001_init.sql:36-38`):

```sql
CREATE TABLE agent_activity (
    agent_account_id    TEXT PRIMARY KEY REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    activity            TEXT NOT NULL,
    activity_at_unix_ms BIGINT NOT NULL
);
```

Interfaces:

```go
// go/internal/runnerhub (the D4 presence state — presence ENUM ONLY, per DL-074)
type PresenceSnapshot struct {
    Presence compassv1.AgentPresence
}
func (h *Hub) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]PresenceSnapshot
// Publish-only hook for live UI (event fan-out) — NOT storage; the durable
// write is Store.SetActivity below.
func (h *Hub) PublishActivity(agentAccountID store.AccountID, activity string) // publishes AgentPresenceChanged{presence, activity}

// go/internal/store (durable activity string — upsert + bulk read over agent_activity)
type AgentActivity struct {
    Activity         string
    ActivityAtUnixMs int64
}
func (s *Store) SetActivity(ctx context.Context, agentAccountID AccountID, activity string, atUnixMs int64) error
func (s *Store) ActivityFor(ctx context.Context, accountIDs []AccountID) (map[AccountID]AgentActivity, error)

// go/internal/store (tree reads; agent_accounts.parent_agent_id already exists per DL-095)
func (s *Store) AgentNeighborhood(ctx context.Context, agentAccountID AccountID) ([]Account, error) // parent + siblings + children
func (s *Store) AgentSubtree(ctx context.Context, agentAccountID AccountID) ([]Account, error)
func (s *Store) AgentsByOwner(ctx context.Context, ownerUserID AccountID) ([]Account, error)

// go/internal/comms (public RPC, account-visibility-scoped via accountVisibleFromWhere)
// GetRoster joins ALL THREE sources: presence enum (in-memory hub) + tree
// (store) + activity string (durable agent_activity table).
func (c *Comms) GetRoster(ctx context.Context, req *compassv1.GetRosterRequest) (*compassv1.GetRosterResponse, error)
```

Two visibility rules exist and DIVERGE; the roster uses ACCOUNT visibility:
the handler clips by `accountVisibleFromWhere` (`accounts.go:478-494` — a
caller sees itself, every user account, its owned agents, and any agent
sharing a `channel_members` channel), while presence EVENTS keep DL-074's
shared-channel rule ("visible to actors who share at least one visible
channel", `compass-notification-delivery/design.md:487-490`). Consequence,
stated rather than papered over: an owner sharing no channel with its agent
sees the agent in the roster (owned) but receives no live
`AgentPresenceChanged` — snapshot-only presence until a channel is shared.

Absent-from-map agents report OFFLINE (matches D4's restart posture);
absent-from-table agents report empty activity. Because the string is
durable, a Server restart does NOT blank it: the roster reads it straight
from Postgres while the hub presence map rebuilds via re-enroll
reconciliation. Red-first tests: neighborhood/subtree/owner scoping; a
non-visible agent never appears (D9); OFFLINE default; activity round-trips
through the DURABLE store (`Store.SetActivity` → `GetRoster`) and the
`AgentPresenceChanged.activity` field; activity SURVIVES A SIMULATED
RESTART — write via `SetActivity`, rebuild the hub state from scratch
(fresh hub, same store), and `GetRoster` still returns the string
(reloaded from the table, not the hub).

### T3 — agent tool leg: `compass_roster` + `compass_set_status` — [compass-agent] SEA-1721

Two new `AgentTool`s in `createCommsTools`
(`compass-agent-comms-tools/design.md:484-485` shape) over the broker; new
relay arms are T1's oneof members, executed by the existing `RelayCommsCall`
handler under `WithActor`.

Interfaces:

```ts
// packages/compass-agent/src/comms.ts
const roster: AgentTool;    // name: "compass_roster", params: { scope?: "neighborhood"|"subtree"|"owner" }
const setStatus: AgentTool; // name: "compass_set_status", params: { activity: string } (server-truncated, cap 140 chars)
```

```go
// go/internal/runnerhub relay dispatch gains the two arms (same switch as post/list)
```

Durability note (resolved OQ-1): the server-side `compass_set_status`
handler first WRITE-THROUGHS the durable `agent_activity` table
(`Store.SetActivity`, T2) — that write COMMITS — and only THEN publishes
`AgentPresenceChanged.activity` as a best-effort live-stream event; unchanged
tool shape, two ORDERED effects (write-then-publish; a lost publish self-heals
on the next `set_status` / reattach re-publish, never leaving the table behind
a live event). The agent-side harness additionally
re-publishes its current status on session (re)attach as a FRESHNESS
mechanism; the durable table, not the re-publish, is the recovery path
after a Server restart.
Red-first: roster tool returns the caller's neighborhood without an explicit
agent id (session-resolved); set_status → durable row upserted AND immediate
`AgentPresenceChanged` with activity; over-cap activity truncated
server-side (the truncated value is what lands in the table).

### T4 — channel policy store + enforcement — [compass-comms] SEA-1722

Migration adds `post_policy`, `owner_account_id`, `mandatory_subscription` to
`channels`; enforcement in the comms handlers.

Interfaces:

```go
// go/internal/store
type ChannelPolicy struct {
    PostPolicy            compassv1.ChannelPostPolicy
    OwnerAccountID        AccountID // empty when OPEN
    MandatorySubscription bool
}
// CreateChannel gains policy; NewChannel struct extends with ChannelPolicy.

// go/internal/comms
// PostMessage: post_policy=OWNER_ONLY && actor != owner_account_id → the same
// in-band not-found/permission error a non-member gets (no oracle).
// UpdateChannelMembers: unsubscribe_account_ids on a mandatory_subscription
// channel → InvalidArgument; owner_account_id/policy fields are server-set,
// never client-mutable via UpdateChannelMembers.

// SetChannelPolicy (owner/operator, create-or-update): the ONLY mutation path
// for post_policy/owner_account_id/mandatory_subscription after creation. Its
// txn seeds the D2 delivery cursor (seed-to-head) for every member the
// mandatory flag newly makes a delivery target — transactional with the flag
// flip, because an un-seeded delivery target is the fail-DANGEROUS hazard D2
// names (compass-notification-delivery/design.md:293-311: seeds are
// transactional with the membership row).
func (c *Comms) SetChannelPolicy(ctx context.Context, req *compassv1.SetChannelPolicyRequest) (*compassv1.SetChannelPolicyResponse, error)
```

D1 subscriber-resolution SQL gains the third disjunct:
`(cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)`.
Red-first: non-owner post rejected in-band; owner post lands; unsubscribe on
mandatory channel rejected; delivery reaches a member whose row says
`subscribed=false` on a mandatory channel (read-side guarantee);
`SetChannelPolicy` flipping `mandatory_subscription=true` on a channel with
unsubscribed members seeds a cursor row for each of them in the same txn
(no un-seeded delivery target).

### T5 — coordination-channel auto-provision + tree reconciliation — [compass-comms] SEA-1722

Hook the two STORE-level writers of `agent_accounts.parent_agent_id`:
`store.CreateAgent` (`accounts.go:131`, INSERT at `:156-158`) and
`store.ReparentAgent` (`accounts.go:280`, UPDATE at `:346-348`). Every
RPC-level path — the spawn chain
(`compass-agent-spawn-despawn/design.md:201-205`), the public `CreateAgent`
RPC (`comms.go:126`), and `ReparentAgent` — is a caller of one of the two,
so the hook is complete by construction, for future writers too (B1).

Interfaces:

```go
// go/internal/store — the parent-edge writers own the tx and invoke a
// REGISTERED hook on it, so the store never imports comms/channel types:
type CoordinationHook func(ctx context.Context, tx pgx.Tx, managerAgentID AccountID) error
func (s *Store) SetCoordinationHook(CoordinationHook) // wired once at startup
// store.CreateAgent / store.ReparentAgent call the hook on their own pgx.Tx
// (from s.pool.Begin, matching validateNewParent at accounts.go:376) right
// after writing parent_agent_id (no-op if unregistered).
// INVARIANT (greppable; repeated at the parent_agent_id column): every write of
// agent_accounts.parent_agent_id must invoke the registered hook.
//
// go/internal/comms — registers a closure running the in-tx reconcile (channel
// upsert + membership rows + D2 cursor seeds on the passed tx); idempotent per
// manager. ChannelChanged is emitted post-commit, best-effort (self-heals on
// the next reconcile / D1 sweep), NEVER inside the tx.
func (c *Comms) reconcileCoordinationTx(ctx context.Context, tx pgx.Tx, managerAgentID store.AccountID) error
func (c *Comms) EnsureCoordinationChannel(ctx context.Context, managerAgentID store.AccountID) (store.ChannelID, error) // manual/backfill entrypoint; wraps its own tx, then post-commit emit
func (c *Comms) ReconcileCoordinationMembership(ctx context.Context, managerAgentID store.AccountID) error // membership-only resync (manual)
```

Provision on FIRST report: channel named `<manager-handle>-coordination` in
the manager's owner's group, `owner_account_id=manager`,
`mandatory_subscription=true`, `post_policy=OWNER_ONLY` (resolved OQ-9),
members = manager + reports (each member add seeds the D2 cursor to channel
head in the same txn, per `compass-notification-delivery/design.md:293-311`).
Reparent-in adds the report; reparent-out removes it (final event via
`ChannelChanged.removed_account_ids`, `comms.proto:461-472`). Losing the last
report leaves the channel dormant, never deleted — accepted MVP accretion
(DL-136). The coordination channel is always provisioned GROUPED — in the
manager's owner's group — so the partial uniqueness index
`channels_group_name_key ON channels (group_id, name) WHERE group_id IS NOT
NULL` (`migrations/0001_init.sql:78`) applies and the collision analysis is
scoped to grouped channels. Name-collision on re-provision resumes the
existing channel ONLY when its `owner_account_id` matches the manager: a user
can manually create `<handle>-coordination` first (handles are globally unique
and no rename RPC exists, so only the same-owner manual collision matters), and
blind resume would adopt an arbitrary user channel and force policy onto it. On
ownership mismatch the reconciler DETERMINISTICALLY SUFFIXES
(`<handle>-coordination-2`, next free) — it NEVER adopts, and it NEVER fails
the parent-edge write: provisioning is best-effort relative to the tree write,
which is authoritative (a user's manually-chosen channel name must never wedge
report creation). Members reach the channel through mandatory subscription, not
by resolving its name, so the suffix is a cosmetic display-label difference on
this pathological same-owner deliberate pre-collision, not a discovery break.
Red-first: first-report provisions exactly once (second report joins, no second
channel); provisioning fires on EACH of the three RPC paths (spawn, public
`CreateAgent`-with-parent, reparent-in) through the same store hook; reparent
moves membership; despawned report keeps membership (account persists,
DL-077); collision with a manager-owned channel resumes; collision with a
user-owned channel does NOT adopt but suffixes, AND the parent-edge write
still succeeds (the reconcile hook never rolls back the spawn/reparent).

### T6 — pinned-board store + `UpdatePinnedBoard` — [compass-comms] SEA-1723

Migration: `channel_pins (channel_id, message_id, position, pinned_at,
pinned_by_account_id, PRIMARY KEY (channel_id, message_id))`, per-channel cap
enforced in the txn (5, resolved OQ-5). NO message writes anywhere in this
task: pinning points, posting stays with `PostMessage` (DL-099, OQ-8).

Interfaces:

```go
// go/internal/store — every board txn opens with
//   SELECT 1 FROM channels WHERE id = $1 FOR UPDATE
// One lock serializes BOTH races: the cap check (count-then-insert in two
// concurrent txns under READ COMMITTED both read 4, both insert -> 6 pins)
// and the repoint compare-and-swap below (C3 + S3, one mechanism).
type PinnedEntry struct {
    MessageID         MessageID
    Position          int32
    PinnedAtUnixMs    int64
    PinnedByAccountID AccountID
}
// PinMessage pins an EXISTING message: validates msg belongs to a topic whose
// channel_id = ch (join through topics — messages carry only topic_id per
// DL-098), then inserts the pin pointer. NO message insert. replace != "" is
// compare-and-swap: replace MUST name a currently pinned message_id or the op
// fails in-band ("board changed, re-read") — two concurrent repoints of the
// same entry resolve to one winner and one explicit retry, matching the
// immutable-message philosophy.
func (s *Store) PinMessage(ctx context.Context, ch ChannelID, msg MessageID, replace MessageID, by AccountID) ([]PinnedEntry, error)
func (s *Store) UnpinMessage(ctx context.Context, ch ChannelID, msg MessageID) ([]PinnedEntry, error)
func (s *Store) PinnedEntries(ctx context.Context, ch ChannelID) ([]PinnedEntry, error)

// go/internal/comms — UpdatePinnedBoard handler: authz = post_policy (owner on
// OWNER_ONLY, any member on OPEN); pure pointer ops, never a Message insert;
// emits ChannelChanged.
func (c *Comms) UpdatePinnedBoard(ctx context.Context, req *compassv1.UpdatePinnedBoardRequest) (*compassv1.UpdatePinnedBoardResponse, error)
```

Red-first: pin of a message living in another channel's topic rejected
in-band; pin-with-replace atomically swaps (old id gone, new id present,
position preserved); replace naming a no-longer-pinned id fails in-band
(CAS); two concurrent cap-edge pins admit exactly one (FOR UPDATE); cap
rejected in-band at cap+1; non-owner pin on OWNER_ONLY rejected;
`ChannelChanged` carries the updated board.

### T7 — pinned-board delivery: the pin sweep — [compass-comms] SEA-1723

Extend the D2 session-start sweep (`compass-notification-delivery/design.md:340-346`)
with a sibling pin step, and let live edits ride D1 unchanged (the edit's new
message is minted by its own topic-mandatory `PostMessage` → a normal
`MessagePosted` on the bus → normal fan-out at settle; the repoint itself is
`Channel` state riding `ChannelChanged`).

Interfaces:

```go
// go/internal/delivery — after the cursor sweep per subscribed channel:
// for each PinnedEntry, dispatch DeliverControl{Message} for the pinned
// message REGARDLESS of cursor position; per-session message_id dedup
// (agent-side, DL-073/T5) absorbs the overlap when the cursor sweep already
// delivered it this session.
func (d *Deliverer) sweepPins(ctx context.Context, agent store.AccountID, sessionID string) error
```

No cursor advance semantics change: a pin-sweep deliver is acked like any
deliver; an ack for an already-below-cursor seq is the existing no-op
(`design.md:338` — "A duplicate or reordered ack is a no-op"). Red-first: fresh
session receives current pins even when `acked_seq` ≥ pin seq; edited board
(new message id) delivers to all subscribers at author settle; a message
pinned and swept in the same session is injected once (dedup).

### T8 — UI render: board strip + policy affordances — [compass-ui] [SEA-1722 + SEA-1723]

- Channel header renders `Channel.pinned_entries` as a compact board strip
  (resolve message ids via the query layer, DL-128); edit/unpin affordances
  gated by `post_policy`/`owner_account_id`.
- Composer disabled with an "owner-only channel" hint when the viewer fails
  the post policy; subscribe toggle hidden on `mandatory_subscription`
  channels.
- Roster: the UI already has presence via `AgentPresenceChanged`; surface the
  new `activity` string in the existing presence renderings (no new view).

Interfaces: generated TS from T1 (`Channel.pinnedEntries`, `postPolicy`,
`mandatorySubscription`, `GetRoster`, `AgentPresenceChanged.activity`);
`@tanstack/solid-query` hooks per DL-128. Red-first (component tests):
non-owner composer disabled; pin strip renders and updates on
`ChannelChanged`.

## Tasks

- [ ] **T1** — proto delta: `ChannelPostPolicy` + `Channel` policy/pin fields,
  `GetRoster`, `AgentPresenceChanged.activity`, `PinnedEntry` +
  `UpdatePinnedBoard` (pin-by-existing-message_id), `SetChannelPolicy`,
  `CommsCallRequest`/`Result` arms (`roster`, `set_status`, `pin`); buf
  lint/breaking clean. [compass-repo] [SEA-1721/1722/1723] — gates T2-T8.
- [ ] **T2** — hub presence snapshot (enum-only), durable `agent_activity`
  table + store activity read/write, store tree reads,
  `GetRoster` handler (account-visibility-scoped, `accountVisibleFromWhere`;
  presence events keep the shared-channel rule). [compass-comms] SEA-1721
- [ ] **T3** — `compass_roster` + `compass_set_status` (durable write-through
  - event publish) tools + relay arms.
  [compass-agent] SEA-1721
- [ ] **T4** — channel policy migration + `PostMessage`/`UpdateChannelMembers`
  enforcement + D1 mandatory-subscription disjunct + `SetChannelPolicy`
  (cursor-seeding txn). [compass-comms] SEA-1722
- [ ] **T5** — coordination-channel auto-provision hooked at the two
  store-level `parent_agent_id` writers (spawn + public CreateAgent +
  reparent by construction) + membership reconciliation, ownership-checked
  resume. [compass-comms] SEA-1722
- [ ] **T6** — `channel_pins` store + `UpdatePinnedBoard` (pointer-only
  pin/unpin/repoint over existing message_ids; CAS repoint + FOR UPDATE
  serialization; cap). [compass-comms] SEA-1723
- [ ] **T7** — session-start pin sweep beside the D2 cursor sweep; live edits
  ride D1 unchanged. [compass-comms] SEA-1723
- [ ] **T8** — UI: board strip, owner-only composer gating, hidden subscribe
  toggle on mandatory channels, activity string in presence renders.
  [compass-ui] [SEA-1722/1723]

## Decisions (ratified by Matt 2026-07-31)

Originally batched as Open Questions (this record's author has no ask path);
Matt ruled on all nine on 2026-07-31. Each entry keeps its original rationale
and now states the resolution. OQ-1 and OQ-9 are OVERRIDES of the drafted
recommendations, folded into the record body (A1/T2/T3 and A2/T5
respectively); the other seven resolve to the drafted positions. No
unresolved fork remains.

### OQ-1 — RESOLVED (Matt 2026-07-31): activity string is agent-set, DURABLE (DB-backed), re-published on reattach for freshness

`AgentPresenceChanged` carries no activity today (`comms.proto:498-503`), and
DL-074's enum is bare 4-state. Options were: (a) **agent-set self-report** —
a `compass_set_status` tool publishing an additive
`AgentPresenceChanged.activity = 3` field; (b) server-derived from the
current turn's message/session state; (c) no activity string in MVP (roster
returns presence only). **Ruling: (a), with the string DURABLE** — Matt
overrode the drafted in-memory storage: the activity string lives in its own
store table (`agent_activity`, T2), written through by `compass_set_status`,
and a Server restart RECOVERS it from Postgres. The agent-set-source
rationale stands: it mirrors the Cotal status precedent DL-074's vocabulary
was aligned to, and costs one tool + one field + one small table; server
derivation guesses, the agent knows. The presence ENUM keeps DL-074's
in-memory/no-heartbeat posture unamended; only the STRING gains a durable
table — a deliberate divergence (DL-074's existing-row note is AMENDED, see
Ledger impact). The draft's restart caveat — an UNRECOVERABLE string,
because DL-074 reconciliation rebuilds presence via `GetAgentStatus`, which
"returns only lifecycle state"
(`compass-notification-delivery/design.md:495-503`) — is mooted by
durability; the agent-side re-publish on session (re)attach is KEPT as a
freshness mechanism (keeps the string current), no longer the recovery path.

### OQ-2 — RESOLVED (Matt 2026-07-31): both-with-a-filter scopes; fleet-wide OWNER visibility RATIFIED

Tree-neighborhood only, owner-wide only, or both-with-a-filter (Matt's card
floats "whole owner? my subtree? Both, with a filter"). **Resolved:
both-with-a-filter**: `RosterScope` enum with `NEIGHBORHOOD` (parent + peers +
children) as the zero-value default — the Manager-model common case — plus
`SUBTREE` and `OWNER`. The disclosure is RATIFIED (S1): "D9 clips
every scope" is NOT a safety argument once A2's `mandatory_subscription`
channels exist — agent-to-agent visibility flows through shared
`channel_members` rows (`accounts.go:478-494`: an agent viewer sees another
agent only via a shared channel membership), so a mandatory
root #announcements-class channel puts EVERY agent in one shared channel, and
OWNER-scope roster then returns the full fleet cross-tree, including each
agent's activity string. Under the manager model, OWNER scope approximately
equals fleet-wide and activity strings are fleet-visible. Matt ratified
exactly this (DL-075: all wave agents share one owner, `DECISIONS.md:132`,
makes it the intended shape): OWNER scope IS fleet-wide, disclosed and
accepted; the tree-clip (subtree-of-root) alternative was not taken.

### OQ-3 — RESOLVED (Matt 2026-07-31): channel-level policy fields

A new `Channel.post_policy` enum + `owner_account_id` + a channel-level
`mandatory_subscription` bool (this record's shape), vs a new `ChannelKind`,
vs a per-member role/locked-subscribe bit. **Resolved: the channel-level
fields**: kind encodes conversation topology (`comms.proto:226-230`), not
authority — a kind forks a matrix; per-member bits invite row drift and don't
express "same policy as #announcements" (all members, always). Channel-level
policy also composes cleanly into SEA-1622's later tree-folding: the policy
travels with the channel wherever the namespace lands, and a later
`OWNER_AND_PARENTS`/role-based enum value is additive. The
`mandatory_subscription` guarantee is enforced read-side in D1's subscriber
resolution (a third disjunct beside the home-channel repair), immune to
row-flag drift.

### OQ-4 — RESOLVED (Matt 2026-07-31): first-report-gained, store-level hook

On Manager spawn vs on first report gained; and ownership across
reparent/despawn. **Resolved: first-report-gained**: most agents never
manage, so spawn-time provisioning mints dead channels. Completeness comes
from the STORE level, not RPC enumeration: `agent_accounts.parent_agent_id`
has exactly two store-level writers — `store.CreateAgent` (INSERT,
`accounts.go:156-158`) and `store.ReparentAgent` (UPDATE,
`accounts.go:346-348`) — and every parent-edge path (the spawn chain, the
public `CreateAgent` RPC with `parent_agent_id` set — `comms.proto:534-537`,
`comms.go:126` — and reparent) funnels through them, so a store-registered
in-transaction hook on those two methods (the store owns the tx and calls the
hook on its own `*tx`; the comms layer registers the reconcile closure — see
A2/T5) covers every current AND future writer by construction. Lifecycle:
membership reconciles on reparent (report follows
its new parent; the departing member gets the final `ChannelChanged` per
`comms.proto:461-472`); despawn changes nothing (DL-077 — the account
persists); a manager losing its last report keeps a dormant channel (history
survives; re-gaining a report resumes it — ownership-checked, T5). If the
MANAGER itself is re-parented, its coordination channel follows it unchanged
— the channel is keyed to the manager, not to its position.

### OQ-5 — RESOLVED (Matt 2026-07-31): per-channel, capped 5, turn-settle

Per-channel vs per-tree; one slot vs a small ordered set; does an edit wake
idle Managers (steer-style interrupt) or land at next turn boundary?
**Resolved: per-channel, capped ordered set (5), turn-settle delivery.**
Per-channel composes (root #announcements AND each coordination channel get
boards from one primitive; "per-tree" is then just "the board of the tree's
coordination channel"). A single slot forces destructive overwrites of
unrelated headlines; an uncapped set becomes a feed. Edits land as ping-class
delivers under DL-071's settle timing — a real emergency already has the
mention→steer path, and the board must not become a second interrupt
mechanism. Edit = topic-mandatory post + atomic CAS repoint (OQ-8), so
redelivery is ordinary delivery (dedup-safe under DL-072/073, no rail changes).

### OQ-6 — RESOLVED (Matt 2026-07-31): compatible with DL-054, no amendment

DL-054 froze "no notifications page, centre, badge, or read state"
(`DECISIONS.md:127`). Judgment: the pinned board is compatible — it is
channel-scoped broadcast content delivered as chat via the existing rail, with
no centre/badge/read-state; the delivery cursor is plumbing, not user-facing
read state. But it IS a new notification-shaped primitive, so the call is
surfaced rather than assumed. **Resolved: compatible, no DL-054 amendment** —
Matt confirmed the judgment; the fallback (scoping SEA-1723 to agent-side
delivery only, no UI board strip) was not needed.

### OQ-7 — RESOLVED (Matt 2026-07-31): SetChannelPolicy ships as the enabler; top-level provisioning stays out of scope

SEA-1722's brief says the coordination channel is "the SAME missing primitive
behind restricted-post #announcements / #incidents". This record designs the
PRIMITIVE (post_policy + mandatory_subscription + board) and the
coordination-channel auto-provisioning; it does NOT auto-provision
top-level #announcements/#incidents channels. **Resolved: provisioning of named
top-level channels stays out of scope, and `SetChannelPolicy` (T1/T4) ships
as the mechanical enabler** — the fields alone are NOT enough for a PRE-EXISTING
channel: they are create-time-set and never client-mutable via
`UpdateChannelMembers` (T4), so an existing #announcements could never gain
policy without recreate-and-lose-history; and flipping
`mandatory_subscription` on a channel with unsubscribed members would mint
delivery targets with NO cursor row — exactly the un-seeded-subscribe hazard
D2 calls fail-DANGEROUS (seeds are transactional with the membership row,
`compass-notification-delivery/design.md:293-311`). `SetChannelPolicy`'s txn
therefore seeds cursors for every member the mandatory flag newly makes a
delivery target; an operator upgrades existing channels in place — no
recreation, no landmine.

### OQ-8 — RESOLVED (Matt 2026-07-31): pin-by-existing-message_id (b)

DL-098 makes posting topic-mandatory, with "no default, general, or catch-all
topic anywhere — channels carry zero messages directly" (`DECISIONS.md:170`);
DL-099 makes `comms_post_message(topic)`/`PostMessage` the ONLY message-write
paths (`DECISIONS.md:171`). Three shapes for pinning: (a) `UpdatePinnedBoard`
gains a `topic` oneof and mints the message in-topic — a second message-write
path, redundant with `PostMessage` and in tension with DL-099; (b) pinning
references an EXISTING `message_id` — the board is a pure pointer set,
"edit" = topic-mandatory post + repoint, two composed ops over existing
paths, no new write path at all; (c) a server-minted per-channel board topic
holding board messages — collides head-on with DL-098's "no … catch-all
topic anywhere" and would need a ledger amendment. **Resolved: (b)** — the
record's drafted position: pinning composes with DL-098/099 instead of
engaging them, and the edit-as-new-message delivery insight survives
unchanged (a new message id rides DL-072/073 dedup + cursor math as-is).

### OQ-9 — RESOLVED (Matt 2026-07-31): OWNER_ONLY + mandatory-subscribe; report→manager flows via DMs/group DMs

The draft flagged the one-way tension: under DL-099 the comms tool is an
agent's ONLY comms-write path (`DECISIONS.md:171`), so on an OWNER_ONLY
channel a report cannot reply, ack, raise an Ask, or post status in the very
channel carrying its manager's directives — and the draft recommended OPEN +
mandatory so reports could reply in-channel. **Matt overrode: OWNER_ONLY +
`mandatory_subscription=true`.** The coordination channel is a ONE-WAY
manager→reports directive surface; the mandatory-subscribe guarantee remains
the load-bearing half of the policy. Rationale (token cost): the vast
majority of manager↔report coordination need not be seen by every other
report — an OPEN coordination channel broadcasts every reply into every
subscribed report's delivery, burning tokens fleet-wide. Where
report→manager traffic flows (closing the draft's one-way caveat): direct
DMs and small targeted group DMs — `CHANNEL_KIND_DM = 1` /
`CHANNEL_KIND_GROUP_DM = 2` (`comms.proto:226-230`), a DM widening "into a
GROUP_DM as members are added" (`comms.proto:223-225`) — e.g. three agents
on one issue open one group DM with exactly those three. STANDING DIRECTIVE
(Matt, captured in A2 and DL-136): agents at every level heavily prefer
direct DMs / small targeted group DMs for coordination, to keep
coordination-token-cost low; the coordination channel carries only broadcast
directives every report must see, never per-report chatter. The PRIMITIVE
(OQ-3) is unchanged — OWNER_ONLY is purely the enum value T5 provisions.
The #announcements-class channels themselves stay OWNER_ONLY (unchanged).

## Ledger impact

Appended to `docs/designs/product/DECISIONS.md` (§ Comms & tools) in the same
PR — DL-135/136/137 (main's max row was DL-128; #1089 (SEA-1732) took the
DL-129..134 block first, so this record shifts to the next free block
(deconflicted via the wave coordinator; the gate has no contiguity check). Status
`Active (Matt, 2026-07-31)`. The rows below are the wording as written there:

- **DL-135** (new, § Comms & tools) — Agent roster is a pull: a public
  `CommsService.GetRoster` (account-visibility-scoped via
  `accountVisibleFromWhere`, tree-derived scopes NEIGHBORHOOD/SUBTREE/OWNER —
  OWNER ratified fleet-wide under the shared-owner model, activity strings
  fleet-visible) plus a native `compass_roster` tool on the
  `CommsCallRequest` relay family, reading the DL-074 in-memory presence
  enum joined with the agent tree; the activity string is DURABLE (its own
  `agent_activity` store table), recovered from Postgres on Server restart —
  agent-set via `compass_set_status` (write-through to the table + additive
  `AgentPresenceChanged.activity`), re-published by the agent-side harness
  on session (re)attach for freshness — a deliberate divergence from
  DL-074's in-memory posture for the STRING; the presence enum stays
  in-memory. Presence EVENTS keep the shared-channel visibility rule while
  the roster uses account visibility (divergence stated: an owner sharing no
  channel with its agent gets snapshot-only presence). SEA-1721
- **DL-136** (new, § Comms & tools) — Channel post authority and forced
  subscription are channel-level policy fields (`post_policy`
  OPEN/OWNER_ONLY + `owner_account_id` + `mandatory_subscription`), never a
  `ChannelKind` or a visibility overload; `mandatory_subscription` is
  enforced read-side as a third disjunct in D1's subscriber resolution; an
  owner/operator `SetChannelPolicy` (create-or-update) is the only
  post-creation mutation path, its txn seeding delivery cursors for every
  member a mandatory flip newly targets. A Manager's coordination channel is
  auto-provisioned on first report gained via a reconciler hooked at the two
  store-level writers of `agent_accounts.parent_agent_id`
  (`store.CreateAgent`, `store.ReparentAgent`) — covering spawn, public
  `CreateAgent`, reparent, and any future writer by construction; resume on
  name-collision is ownership-checked (never adopts a user-created channel);
  membership reconciles with tree edges; dormant channels are never deleted
  (accepted MVP accretion). The coordination channel is OWNER_ONLY +
  mandatory-subscribe — a one-way manager→reports directive surface;
  report→manager and lateral coordination flows through DMs/group DMs, and
  agents at every level heavily prefer direct DMs/small targeted group DMs
  to keep coordination-token-cost low (standing directive, Matt). Precursor
  primitive to SEA-1622, not part of it. SEA-1722
- **DL-137** (new, § Comms & tools) — The pinned board is a server-side
  per-channel capped ordered POINTER set over existing topic-scoped
  messages: pinning references an existing `message_id` (validated to a
  topic of the channel, join through `topics`) and creates no `Message`, so
  DL-099's single-write-path stands; edit = a normal topic-mandatory post +
  compare-and-swap repoint (redelivery is ordinary DL-071/072/073 delivery
  of a new message id, dedup-safe); board txns serialize on `channels … FOR
  UPDATE` (cap + repoint races); startup delivery is a session-start pin
  sweep beside the D2 cursor sweep dispatching current pins regardless of
  cursor position; edits land at turn-settle (never steer); explicitly
  distinct from DL-096's client-local sidebar pins, and compatible with
  DL-054 (no centre/badge/read state). SEA-1723

Existing rows touched (cited; DL-074 AMENDED, none superseded):

- **DL-054** — positioned compatible; confirmed by Matt, no amendment
  (OQ-6 resolved).
- **DL-071/072/073** — consumed as the delivery rail, unchanged.
- **DL-074** — AMENDED for the activity string: the presence 4-state enum
  stays in-memory (unchanged); the agent-set activity string gains a durable
  `agent_activity` table — a deliberate divergence from DL-074's
  no-durable-table posture, ratified by Matt 2026-07-31.
- **DL-075/077** — spawn/despawn semantics consumed by T5's lifecycle rules.
- **DL-095** — the tree remains the organizing primitive; roster and
  coordination channels derive from it; SEA-1622 relationship restated
  (precursor, not part-of).
- **DL-096** — cited only to disambiguate; untouched.
- **DL-098** — composed with, not amended: pins point at topic-scoped
  messages (a pin's channel is reached through `topics.channel_id`); no
  board topic, no catch-all topic, no channel-direct message.
- **DL-099** — composed with, not amended: pinning creates no message, so
  the single comms-write path (`comms_post_message`/`PostMessage`) stands;
  board edits post through it, then repoint.
