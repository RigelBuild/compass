# Compass channels in the agent tree

Status: Active

Tracker: RIG-1622

This record is filed under `ui` because the change is driven by the
left-sidebar hierarchy and compass-ui owns that surface, but it cross-cuts
the proto contract (`proto/compass/v1/comms.proto`) and the Go comms
service. Tasks below name the owning lane per slice.

## Problem / Intent

The left sidebar renders two parallel hierarchies: a Channels section
partitioned by `ChannelGroup` rows and an Agent workspaces section derived
from `AgentAccount.parent_agent_id`
(`apps/ui/src/components/LeftSidebar.tsx:414-416`: "then two collapsible
sections — Channels above Agent workspaces"). The frozen agent-trees record
names this work: "this record makes the agent tree the primitive RIG-1622
will fold channels into" (`docs/designs/agent/compass-agent-trees/design.md:233-234`)
and fences it off: "channels stay on their own `ChannelGroup` tree until
RIG-1622" (`design.md:297-298`). Fold channels into the agent tree so the
sidebar shows one hierarchy and posting boundaries follow the tree.

## Global Constraints

- **Additive wire change only.** No proto field is removed or renumbered, so
  no `reserved` statement is needed. The one removed-field precedent is
  explicitly pre-dogfood: "the oneof and field 7 parent_message_id are both
  REMOVED, not reserved (F9: pre-dogfood, zero stored payloads)"
  (`proto/compass/v1/comms.proto:341-344`). This design removes nothing.
- **Visibility predicate copies stay textually identical.** The store repeats
  the effective-visibility CTE per read: "The copies MUST stay textually
  identical so the stream edge's single-id visibility check cannot drift from
  the list read" (`go/internal/store/queries/channels.sql:10-12`). Any
  predicate change lands in every copy in one commit. The same anti-drift
  rule already binds the membership probe this design widens:
  "requireChannelMember / isChannelMember reuse ChannelMemberExists
  (channels.sql) — the statement is textually identical"
  (`go/internal/store/queries/authz.sql:4-5`).
- **The coordination-hook invariant governs the agent edge only.** The
  invariant "every write of parent_agent_id must invoke the registered
  coordination hook (RIG-1722 T5)"
  (`go/internal/store/migrations/0001_init.sql:86-87`) names
  `agent_accounts.parent_agent_id`, whose two writers are `CreateAgent`
  (`go/internal/store/accounts.go:363-372`) and `ReparentAgent`
  (`accounts.go:614-633`). It holds unchanged: the hook reconciles the
  stored membership of coordination channels, which stay
  explicitly-membered. `channels.parent_agent_id` is a NEW edge with its
  own writers (`CreateChannel` with attach, `ReparentChannel`); the hook
  never fires on those writes, and a tree-derived channel needs no
  membership write on any tree move (Approach leg 3), so this design
  extends no hook.
- **Not-found/forbidden merge on every authz failure.** "a non-member gets
  ErrNotFound (the not-found/forbidden merge), never a hint"
  (`go/internal/store/messages.go:56`). New authz branches keep the merge.
- **The caller is never a request field.** "the caller is the account
  authenticated on the connection … never a field in a request, which would
  be spoofable" (`proto/compass/v1/comms.proto:27-29`).
- **Reserved namespaces are untouchable.** The per-owner `__dm__` group
  (`go/internal/store/dm.go:19`) and the `__coordination__` group it is
  "distinct from" (`dm.go:15`) keep their storage shape; OpenDM and the
  coordination reconcile depend on them.

## Approach

**A channel gains an optional owning-agent edge and a membership mode;
`ChannelGroup` survives as server machinery only.** The recommended shape
has five legs.

### 1. What replaces `ChannelGroup`

Add `Channel.parent_agent_id` (new field 11; field 10 is the last used,
`repeated PinnedEntry pinned_entries = 10;` at `proto/compass/v1/comms.proto:259`).
Empty = a tree-root channel. When set, the channel hangs off that
`AgentAccount` in the one sidebar tree. The field name follows the existing
vocabulary: `AgentAccount.parent_agent_id` already "Mirrors
ChannelGroup.parent_group_id" (`comms.proto:188`).

`ChannelGroup` is NOT deleted. Two server subsystems depend on group rows as
storage machinery, not as a user-facing hierarchy:

- OpenDM ensures "the owner's reserved `__dm__` group, then upsert the"
  DM channel under it (`go/internal/comms/dm.go:34`; the reserved name
  constant is `go/internal/store/dm.go:19`).
- The coordination reconcile keeps a manager's channel provisioned from tree
  edges, in a `__coordination__` group `__dm__` is "distinct from"
  (`go/internal/store/dm.go:15`).

What changes is the group's role: it stops being the sidebar organizer and
stops accepting user-facing nesting. `CreateChannelGroup` remains wire-legal
(additive contract, Global Constraints) but the UI drops it as an
organizing surface; a group survives as a flat namespace for reserved
machinery and for SHARED spaces (leg 2).

An agent's home channel needs no new edge at all: it is already joined to
its agent by `AgentAccount.home_channel_id`, "minted at CreateAgent"
(`comms.proto:182-184`), created "ungrouped (owner-scoped)"
(`go/internal/store/accounts.go:375`). The UI derives its placement under
the agent node from that existing field.

### 2. The visibility invariant

This is the load-bearing decision. Today the lattice lives on groups: "This
group's own visibility; the server rejects a value more open than the parent
group's (child ≤ parent). Effective visibility is the most restrictive on
the path to the root" (`comms.proto:214-216`), enforced at write
(`go/internal/store/channels.go:48`: `if int32(g.Visibility) > int32(parentVis)`)
and computed at read by a recursive CTE
(`go/internal/store/queries/channels.sql:130-142`:
`LEAST(a.min_vis, g.visibility)` then `MIN(min_vis) AS eff_vis`).
`AgentAccount` has an owner (`owner_user_id`, `comms.proto:181`) but no
visibility field.

**Decided (Matt, RIG-1622): by default every agent under an owner reads
every channel of that owner; a later ACL system will gate individual
channels off.** The read grant below is that product decision. It is NOT a
reuse of existing predicate behaviour: today the owner-set `viewer` CTE
("SELECT owner_user_id AS uid FROM agent_accounts WHERE account_id = $1",
`channels.sql:94`) exists only in the two GROUP queries (`channels.sql:93`,
`:118`, applied at `:101` and `:126`); all three CHANNEL predicate copies
grant non-member access solely through the SHARED-group arm (`c.kind = 0
AND c.group_id IS NOT NULL AND … e.eff_vis = 1`, `channels.sql:152-153`,
`:181-182`, `:211-212`). The disjunct this record adds is therefore a new
visibility class for agent-attached channels, justified by the ruling, not
by precedent. Precisely:

- **Read grant.** A channel with `parent_agent_id` set is visible to its
  members OR to the anchor agent's owner set: the agent's `owner_user_id`
  plus every agent with that `owner_user_id`. The three channel predicate
  copies gain the `viewer` CTE (the group queries' shape) plus one
  disjunct resolving the anchor's owner through `agent_accounts`. All
  three copies change in one commit (Global Constraints).
- **Visibility is not readability.** Message reads keep their membership
  gate — `ListMessages` joins members ("JOIN channel_members cm ON
  cm.channel_id = t.channel_id AND cm.account_id = $1",
  `go/internal/store/queries/messages.sql:71`) — exactly as a SHARED
  channel is browsable today without message access. A non-member in the
  owner set sees the channel row (sidebar, browse/join); reading history
  and posting require membership, explicit or derived (leg 3).
- **Attachment bounds the non-member grant, not total readership.**
  Members read regardless of owner: cross-owner member sets exist — a
  converted DM keeps both owners' accounts, and `CreateChannel` adds "for
  each agent in the requested member set that agent's owning user(s)"
  (`go/internal/store/channels.go:78-79`). Attachment caps what a
  NON-member can see at the owner set; it never subtracts membership.
- **SHARED channels still cannot hang on an agent.** The owner-set grant
  is the widest non-member access an agent-attached channel can carry;
  shared spaces stay in SHARED groups at the tree root, served by the
  existing SHARED arm.
- **ACL forward-compatibility.** The future per-channel ACL slots in as
  one more conjunct inside the same three predicate copies and the
  participant probe of leg 3 (e.g. `AND NOT EXISTS (SELECT 1 FROM
  channel_acl …)`). The identical-copies rule (`channels.sql:10-12`)
  keeps the insertion surface to exactly those places, so nothing here
  hardens against an ACL landing later. The ACL itself is out of scope.

### 3. Two membership modes; posting follows the tree

**Decided (Matt, RIG-1622): a channel is either tree-derived — its member
set computed from the agent subtree it hangs on, with no managed member
list — or explicitly managed, keeping today's stored `channel_members`
behaviour.** Matt's reasoning: tree-only membership is the right default
for tree-hung channels, but some channels need individually managed
members, so both modes must exist.

**Encoding: a dedicated membership-mode field, not a `ChannelKind` value
and not an implication from placement.** Three candidates were weighed:

- **(a) Explicit mode enum — chosen.** `ChannelMembershipMode` on
  `Channel` (proto field 12; column `channels.membership_mode`), values
  `EXPLICIT = 0` (default, today's behaviour) and `TREE = 1`. Membership
  management is its own axis, independent of both kind and placement.
- **(b) Mode implied by `parent_agent_id IS NOT NULL` — loses.** It
  forecloses an explicitly-managed channel that still lives in the tree,
  which is exactly Matt's individually-managed case composed with
  placement: a user should be able to hang a hand-membered channel under
  an agent for organization. Placement and membership policy must vary
  independently.
- **(c) New `ChannelKind` value 3 — loses.** `kind` is the DM-vs-channel
  axis: `CHANNEL_KIND_CHANNEL = 0`, `CHANNEL_KIND_DM = 1`, and the
  retired `CHANNEL_KIND_GROUP_DM = 2 [deprecated = true]`
  (`proto/compass/v1/comms.proto:289-294`) — whose retirement comment is
  itself the cautionary tale for widening that enum. A tree channel IS a
  `CHANNEL` in every kind-switch in the codebase; a third value would
  make every kind check grow an arm that behaves as `CHANNEL`.

A TREE channel MUST have an anchor: `CHECK (membership_mode = 0 OR
parent_agent_id IS NOT NULL)` (leg 4). An EXPLICIT channel may be
attached or root.

**The derived member set.** For a TREE channel anchored at agent `A`:
`A`, every agent in `A`'s subtree, and `A`'s `owner_user_id`. Subtree
semantics are what make "posting boundaries follow the tree" literal: an
agent may post to TREE channels anchored at itself or at any of its
ancestors. The set is owner-bounded because a reparent cannot cross an
owner boundary: `validateNewParent` rejects a cross-owner parent
(`go/internal/store/accounts.go:666-673`) and its sole caller is
`ReparentAgent` (`accounts.go:594`). Note the scope precisely — this is
a `ReparentAgent` guard, NOT a store-wide invariant: `CreateAgent`'s
store path writes `parent_agent_id` with no cycle or same-owner check at
all (`accounts.go:333-352` — the `InsertAgentAccount` call relies on the
FK, whose only bespoke handling is the
`agent_accounts_parent_agent_id_fkey` missing-referent arm at `:346-348`),
and the same-owner check on that path lives at the comms EDGE
(`go/internal/comms/comms.go:134-140`). So the proto comment
"The server validates same-owner and no-cycle on every write"
(`comms.proto:188-189`) describes the edge, not the store, and is NOT
cited here as a store invariant.

**The gate mechanics: a new probe beside the old one.**
`requireChannelMember` / `isChannelMember` wrap the `ChannelMemberExists`
probe (`SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id =
$1 AND account_id = $2);`, `go/internal/store/queries/channels.sql:36-37`).
A NEW query `ChannelParticipant` is added — the explicit arm OR a tree
arm that walks the actor's ancestor chain and matches the anchor, or
matches the actor as the anchor's owner — and the two wrappers are
rebound onto it.

**`ChannelMemberExists` SURVIVES; it is not replaced.** Two callers
bypass the wrappers and use the sqlc query directly, and both genuinely
mean "has a stored member row", not "participates":

- `hasGenuineAdd` (`go/internal/store/channels.go:550`) drives the R4
  DM-conversion decision — "an update that is not a remove and not an
  unsubscribe, naming an account not already a member" — which is a
  statement about rows in this tx's snapshot. Giving it the derived arm
  would make an add of an already-derived participant a non-add and
  silently skip a conversion.
- The OWNER_ONLY coherence check (`channels.go:765`) requires "the owner
  MUST be a member of the channel", because "the post gate demands the
  author be BOTH a member AND the owner" (`channels.go:757-775`).

**Decided: `SetChannelPolicy` refuses `OWNER_ONLY` on a TREE channel**
(`ErrInvalidArgument`), rather than converting `channels.go:765` to a
participant check. Reason: the coherence check exists to keep an
OWNER_ONLY channel postable, and on a TREE channel it cannot do that job
honestly — the owner's participation is derived from `parent_agent_id`,
so a later move can silently un-participate the named owner and render
the channel unpostable with no write to the policy at all. Precisely
which move, because the two owner cases differ. When the policy owner is
a subtree AGENT, both moves can do it: a `ReparentAgent` carrying that
agent out of the subtree, or a `ReparentChannel` re-anchoring the
channel elsewhere. When the policy owner is the anchor's
`owner_user_id` — the case the owner term in the derived set exists for
— `ReparentChannel` canNOT un-participate them, because the source
authz rule below requires the destination agent's owner to equal the
caller's resolved owner, so every legal destination is same-owner and
the user stays a participant. The agent-owner case alone settles it: a
check that a subsequent, unrelated move can invalidate is not a
coherence check. The refusal is the same shape T5 already uses for
`mandatory_subscription` on TREE, and it keeps `channels.go:765`
reachable only where member rows genuinely exist.

**The refusal binds BOTH writers.** Nothing in the argument above is
specific to `SetChannelPolicy`. `CreateChannel` can mint the same
incoherent state directly, and its own coherence check cannot catch it:
that check is `if c.Policy.OwnerAccountID != "" &&
!slices.Contains(members, c.Policy.OwnerAccountID)` (`channels.go:170`)
against the `expandOwnerMembership` result (`channels.go:160`), and a
TREE create writes no member rows, so that expansion is not the
channel's member set and the check passes vacuously. So `CreateChannel`
refuses `OWNER_ONLY` together with `membership_mode = TREE`
(`ErrInvalidArgument`, T2), exactly as it already refuses
`mandatory_subscription` on TREE.

**A TREE create still runs `expandOwnerMembership`, but writes no member
rows from it.** The expansion is not skipped: the attach authz needs the
resolved owner set, and the expansion result is what `CreateChannel`
carries back today as `MemberAccountIDs` (`channels.go:211`). What a TREE
create skips is the `EnsureChannelMember` loop over it
(`channels.go:174-184`). The list it returns is instead the derived
participant set — the same materialization T4 applies on every read
(leg 5) — so a TREE `CreateChannel` and a later `ListChannels` report
the same member list for the same channel. T4's hop (v) makes that
identity structural rather than a coincidence of two code paths
agreeing: `CreateChannel` stops hand-writing the returned `Channel` at
`channels.go:206-213` and returns the same post-commit `getChannel`
read a `ListChannels` row goes through, so "the same member list" is
the same projection, not a reconstruction of it. The expansion at
`channels.go:160` still runs — the authz needs it — it just no longer
feeds the return value.

**Invariant: `membership_mode` is immutable after create.** This is
load-bearing, not a deferred nicety: it is the sole reason the two TREE
policy refusals above cannot be bypassed by creating an EXPLICIT
channel, setting `OWNER_ONLY` or `mandatory_subscription` on it, and
then converting it to TREE. Today the property holds by absence of a
writer — T1 adds `membership_mode` to `CreateChannelRequest` only, and
`SetChannelPolicy` writes the post policy, the owner and the mandatory
flag and nothing else (`UpdateChannelPolicy`, `channels.go:777-782`) —
and "safe because no writer exists" is exactly the property a later task
deletes without noticing. So it is stated here as an invariant and
guarded: no RPC may write `membership_mode` after the create, and
`ReparentChannel` moves placement only. T2 pins it with a test. Open
Question 3 records the conversion FORK; the invariant itself is not
open.

Sketch of the new probe (the recursive-CTE precedent is the `ancestry`
CTE, `channels.sql:130-137`):

```sql
SELECT EXISTS (
    SELECT 1 FROM channel_members
    WHERE channel_id = $1 AND account_id = $2
) OR (
    -- Mode test HOISTED out of the recursion: an EXPLICIT channel never
    -- enters the CTE, so the rejection path on the post gate stays a
    -- single indexed lookup.
    EXISTS (SELECT 1 FROM channels
            WHERE id = $1 AND membership_mode = 1)
    AND EXISTS (
        WITH RECURSIVE chain AS (
            SELECT account_id, parent_agent_id
            FROM agent_accounts WHERE account_id = $2
            UNION
            SELECT a.account_id, a.parent_agent_id
            FROM agent_accounts a
            JOIN chain ch ON a.account_id = ch.parent_agent_id
        )
        SELECT 1 FROM channels c
        WHERE c.id = $1 AND (
            c.parent_agent_id IN (SELECT account_id FROM chain)
            OR $2 = (SELECT owner_user_id FROM agent_accounts
                     WHERE account_id = c.parent_agent_id)
        )
    )
);
```

**Termination: `UNION`, not `UNION ALL`, because the data is not
trusted.** The walk is actor-to-root and normally bounded by tree depth,
but nothing in the store guarantees acyclic rows — as established above,
the no-cycle guard is a `ReparentAgent` guard only. The Go precedent this
mirrors makes the same assumption explicitly: it carries a visited set
with the comment "The visited set bounds the walk so a pre-existing cycle
in the data cannot spin it forever" (`accounts.go:678-679`), and on
meeting a cycle it `break`s rather than rejecting (`accounts.go:686-689`)
— that is, the code positively contemplates cyclic rows existing. Postgres
does no cycle detection on `UNION ALL`, so `UNION ALL` here would spin
forever inside the post-gate transaction. `UNION`'s distinct semantics
terminate on a repeated row, which is the SQL equivalent of the Go
visited set. (The `ancestry` precedent at `channels.sql:130-137` does not
transfer: it walks `channel_groups`, which the repo treats as immutable
after create — stated there as a load-bearing soundness condition,
"Sound only because groups are immutable post-create: the sole
channel_groups mutation is the CreateChannelGroup INSERT (no
UpdateChannelGroup / re-parent RPC)"
(`go/internal/store/queries/authz.sql:19-22`).)

**Cost on the rejection path.** `AppendMessage` runs this probe in-tx on
every post (`messages.go:59`), so the mode test is hoisted OUT of the
recursion above: a post by a non-member of an EXPLICIT channel resolves
with one indexed `channels` lookup and never materializes an ancestor
chain. Only a TREE channel pays the recursion, and then only for the
actor's own chain (tree depth). Written the other way — the mode filter
inside the recursive `EXISTS` — every rejected post in the system would
build the actor's full ancestor chain first.

**Two walks, in opposite directions.** The probe above answers "is THIS
actor a participant of THIS channel?" and walks UP: the walk is
actor-to-root, seeded `WHERE account_id = $2`. Several sites downstream
ask the opposite question — "who are ALL the participants of THIS
channel?" — and are keyed by channel with no actor to seed from
(`SubscribedAgents` and `ChannelAgentMembers` take an account parameter
only to EXCLUDE the author, `delivery_reads.sql:15`, `:23`). That
question needs a second, DOWNWARD walk, seeded at the channel's anchor.
It is a different recursion, not a re-parameterization of the probe's,
so it is specified here in full rather than referred to:

```sql
-- Anchor-to-subtree DESCENT: the participant set of one TREE channel.
WITH RECURSIVE subtree AS (
    SELECT account_id
    FROM agent_accounts
    WHERE account_id = (SELECT parent_agent_id FROM channels WHERE id = $1)
    UNION
    SELECT a.account_id
    FROM agent_accounts a
    JOIN subtree s ON a.parent_agent_id = s.account_id
)
SELECT account_id FROM subtree
UNION
SELECT aa.owner_user_id
FROM channels c
JOIN agent_accounts aa ON aa.account_id = c.parent_agent_id
WHERE c.id = $1;
```

The recursive step reads `agent_accounts` by `parent_agent_id`, which is
the direction 0001 indexes for exactly this: "The 'children of this
parent' read direction for the agent tree",
`CREATE INDEX agent_accounts_parent_idx ON agent_accounts
(parent_agent_id);` (`0001_init.sql:118-119`). `UNION`, not `UNION ALL`,
for the same reason as the ascent: the store does not guarantee acyclic
rows, and a descent through a cycle spins forever under `UNION ALL`. The
trailing `UNION` adds the anchor's `owner_user_id`, the second disjunct
of the probe. So both walks compute the SAME set — `A`, `A`'s subtree,
and `A`'s `owner_user_id` — read from opposite ends, and the two
rewrites below cannot disagree on a row.

**The descent has two forms, and each site uses the one its key
demands.** The sketch above is the SINGLE-CHANNEL form: it takes one
`channel_id` as `$1` and projects bare `account_id`s, which is all a
caller holding exactly one channel needs. The two channel-keyed delivery
queries T5 rewrites use that form, each being called with a single
`channel_id` (`delivery_reads.sql:13`, `:22`).

`loadChannelMembers` cannot. It is handed a whole id set and must
attribute every returned account back to the channel it belongs to — its
loop keys each row by `m.ChannelID` through
`idx := byID[ChannelID(m.ChannelID)]` (`channels.go:895-902`) — and its
contract forbids one query per channel (leg 5). So it uses the ID-SET
form, which carries the originating channel id through the recursion and
projects attributable `(channel_id, account_id)` PAIRS. A union of
several anchors' subtrees projecting bare accounts would merge two TREE
channels' participants with no way to tell them apart, which is a wrong
ANSWER, not a slow one:

```sql
-- Anchor-to-subtree DESCENT, ID-SET form: the participant set of every
-- TREE channel in $1, each row attributed to its own channel.
WITH RECURSIVE subtree AS (
    SELECT c.id AS channel_id, c.parent_agent_id AS account_id
    FROM channels c
    WHERE c.id = ANY($1::text[]) AND c.membership_mode = 1
    UNION
    SELECT s.channel_id, a.account_id
    FROM agent_accounts a
    JOIN subtree s ON a.parent_agent_id = s.account_id
)
SELECT channel_id, account_id FROM subtree
UNION
SELECT c.id AS channel_id, aa.owner_user_id AS account_id
FROM channels c
JOIN agent_accounts aa ON aa.account_id = c.parent_agent_id
WHERE c.id = ANY($1::text[]) AND c.membership_mode = 1;
```

It is the same recursion in the same direction over the same index; only
the seed and the projection widen by one column. Two properties carry
over unchanged and one strengthens. The `membership_mode = 1` hoist moves
INTO the seed, so an EXPLICIT channel in the id set never enters the
recursion. The trailing `UNION` adds each anchor's `owner_user_id`
against its own `channel_id`, so the owner term stays attributed too.
`UNION` still terminates, and on the pair rather than the account: a
cycle re-emits an already-seen `(channel_id, account_id)` row, the
working table empties and the recursion halts — and because the pair is
per-channel, one channel's cycle cannot truncate another's subtree.

Cost differs by direction, and the descent is the more expensive one:
the ascent is bounded by tree DEPTH (one actor's ancestor chain), the
descent by subtree SIZE. The `membership_mode = 1` filter hoists out of
the descent the same way, so an EXPLICIT channel never enters it.

Because both wrappers are rebound, the derived arm lands on every wrapper
caller at once. The callers were enumerated by grep over `go/**/*.go` for
`isChannelMember(` / `ChannelMemberExists(`, not by recall:

- `AppendMessage` — the post gate (`go/internal/store/messages.go:59`),
  keeping the not-found/forbidden merge ("never a hint that the channel
  exists", `messages.go:56`). The old draft's promise that the post gate
  stays unchanged is withdrawn: the gate gains the derived arm.
- `UpdateChannelMembers` (`go/internal/store/channels.go:387`) — plus the
  TREE-mode refusals below.
- `SetChannelPolicy` (`channels.go:704`).
- `requireBoardMutator` — pin-board mutations
  (`go/internal/store/channel_pins.go:179-180`); its OWNER_ONLY owner
  gate is unchanged.
- `ListTopics` (`go/internal/store/topics.go:23`) — it calls the
  UNEXPORTED `isChannelMember` directly, so it is a separate caller from
  the stream filter below and inherits the derived arm the same way. T3
  carries its acceptance case; without naming it here it would gain
  derived membership with no test.
- The stream filters `IsChannelMember` (`go/internal/store/authz.go:45`)
  and `IsTopicChannelMember` (`authz.go:70`; its
  `TopicChannelMemberExists` query at `authz.sql:8-10` gains the same
  arm with the channel resolved through `topics.channel_id`).

The two direct `ChannelMemberExists` callers above (`channels.go:550`,
`:765`) are deliberately NOT in this list — they keep member-row
semantics.

Read paths that join `channel_members` directly switch to the same
participant shape: `GetPageCursorSeq` (`messages.sql:64`), `ListMessages`
(`:71`), `SearchMessages` (`:81`), `FindAskMessage` (`:92`),
`UpdateMessageBlocksAsAuthor` (`:53`), and `ResolveTopicForUpdate`
(`go/internal/store/queries/topics.sql:16`). Two member-row oracles stay
unchanged and are accepted as under-inclusive for TREE channels in v1:
`SharesVisibleChannel` (`go/internal/store/queries/presence_reads.sql:17-18`)
and the visible-accounts arm (`go/internal/store/queries/accounts.sql:133-134`)
— two accounts related ONLY through a TREE channel are not mutually
visible through them. Stated, not hidden.

**No seeding; no reconcile.** The `SeedHomeChannelMembers` pattern
(`VALUES ($1, $2, FALSE), ($1, $3, TRUE)`,
`go/internal/store/queries/accounts.sql:41-42`) applies to EXPLICIT
channels only. A TREE attach writes no member rows, and a
`ReparentAgent` needs NO membership write for TREE channels — the
subtree is recomputed at query time, so the move commits the agent edge
and nothing else. The coordination hook keeps existing solely for the
coordination channels' stored membership (Global Constraints).

**`UpdateChannelMembers` on a TREE channel is rejected, not ignored.**
An add or remove returns `ErrInvalidArgument` (the actor is a derived
member, so channel existence is already known to it; no merge needed). A
subscription toggle is allowed — see next.

**Subscription state.** `channel_members.subscribed` is a per-row column
(`go/internal/store/migrations/0001_init.sql:219`) and TREE channels
have no rows, so subscription needs a new home. Three options weighed:

- **Full subscription rows for every derived member — loses.** It mints
  a row per (channel, subtree agent) and must rewrite them on every
  reparent: the reconcile the ruling just eliminated, back under another
  name.
- **Derive subscription from the tree (all derived members subscribed) —
  loses.** Every post to a TREE channel would hit every subtree agent's
  turn-end delivery; the existing `mandatory_subscription` class already
  covers "everyone gets it", and the stored default today is
  unsubscribed (`EnsureChannelMember` inserts `FALSE`,
  `accounts.sql:44-46`).
- **Override rows only — chosen.** A new table
  `channel_subscriptions (channel_id, account_id, subscribed)` holds a
  row ONLY where an account explicitly toggled; default is unsubscribed.
  An override row is effective only while the account is still a derived
  member (the delivery queries conjoin the participant check), so a row
  left behind by a reparent-out is inert — no cleanup write is needed on
  any tree move, and a lazy GC may prune later. The subscribe toggle
  seeds the D2 delivery cursor, the same seed-at-subscribe discipline
  the explicit path uses ("insert when it is subscribed (D2
  seed-at-subscribe)", `go/internal/store/channels.go:642`).

**Delivery: the driving relation changes, not a predicate.** This is the
consequence the old draft got wrong, so it is stated precisely. There are
FIVE delivery-side membership sites, and every one of them reads
`FROM channel_members cm` as its DRIVING relation. Which of the two
walks above each one needs is decided by what it is KEYED on, so the key
is tabled beside the site:

| Query | File:line | Keyed on | Walk | Consumer |
| --- | --- | --- | --- | --- |
| `SubscribedAgents` | `delivery_reads.sql:10` | `cm.channel_id = $1` (`:13`) | descent | turn-end delivery fan-out |
| `ChannelAgentMembers` | `delivery_reads.sql:18-24` | `cm.channel_id = $1` (`:22`) | descent | @mention routing |
| `SweepChannels` | `delivery_reads.sql:41` | `cm.account_id = $1` (`:44`) | ascent | the D1 sweep set |
| `UndeliveredMessages` | `delivery_cursors.sql:83` | `cm.account_id = $1` (`:90`) | ascent | undelivered replay |
| `InSweepSet` | `delivery_cursors.sql:102` | `cm.account_id = $1 AND cm.channel_id = $2` (`:105-106`) | ascent | sweep-set membership probe |

A TREE channel has zero `channel_members` rows by construction, so each
of these yields the EMPTY SET before any `WHERE` clause runs. Extending
the existing subscription disjunct (`delivery_reads.sql:14`, `:45`;
`delivery_cursors.sql:91`, `:107`) can therefore never admit a derived
member: a predicate cannot filter a row into existence. What is required
is a rewrite of each query's FROM: a `participants` CTE that UNIONs the
stored `channel_members` rows with the derived participant set, LEFT
JOINing `channel_subscriptions` to supply `subscribed` for the derived
arm. The existing disjunct then reads `subscribed` off that CTE
unchanged.

The derived arm of that CTE is the ASCENT for the three account-keyed
sites and the DESCENT for the two channel-keyed ones. The split is
forced, not stylistic: an account-keyed query is asking "which of THIS
actor's channels does it participate in", which the ascent answers from
the actor it already has; a channel-keyed query is asking "who are all
the participants of THIS channel", and the ascent cannot answer it
because it has no actor to seed from — `SubscribedAgents`' and
`ChannelAgentMembers`' only account parameter is the author to EXCLUDE
(`cm.account_id <> $2`, `delivery_reads.sql:15`, `:23`). `InSweepSet`
takes both keys and uses the ascent, because it is a single-actor probe
and the ascent is bounded by depth rather than subtree size.

`subscribed` is NULLABLE on the derived arm and MUST be coalesced: the
CTE selects `COALESCE(cs.subscribed, FALSE) AS subscribed` from the LEFT
JOIN, so a derived participant with no override row reads FALSE. Without
the COALESCE the arm reads NULL and the disjunct
`(cm.subscribed OR cm.channel_id = aa.home_channel_id OR
ch.mandatory_subscription)` (`delivery_reads.sql:14`) evaluates to NULL,
which a `WHERE` treats as not-true — the same answer today, but only
because the other two disjuncts are structurally FALSE on a TREE
channel: home channels stay EXPLICIT (below) and `mandatory_subscription`
is refused on TREE (below). Both are separate decisions elsewhere in this
record, so relaxing either would silently flip delivery for every
un-overridden derived participant. The COALESCE makes the CTE's own
semantics total and independent of them.

`ChannelAgentMembers` has no subscription predicate and needs the union
alone; it is the site that carries @mentions — `resolveMentioned`
(`go/internal/delivery/dispatch.go:278`) reads it for both the reserved
`@everyone`/`@agents` expansion and the per-handle membership check, and
`dispatch.go:272-273` states "a resolved agent that is not a channel
member is also a no-op", so without this rewrite every mention in a TREE
channel is silently dropped. T5 owns all five.

One further channel-keyed site is deliberately NOT rewritten:
`SeedChannelDeliveryCursors` (`delivery_cursors.sql:15-22`, keyed
`WHERE cm.channel_id = $1` at `:21`) would need the descent by the same
argument, but its two callers both fire only on a mandatory channel
(`channels.go:198`, `:795`) and `mandatory_subscription` is refused on
TREE, so it can never see a mode-1 channel in v1. Stated so a later
relaxation of that refusal knows this query is the sixth site.

**Cost, stated honestly.** This puts a recursive CTE on the delivery
fan-out path, evaluated per post, where today's shape is a
`channel_members` index scan (`0001_init.sql:216-222`). The ascent is
one actor's ancestor chain — depth of the agent tree, small; the descent
is the anchor's whole subtree, so the two fan-out sites pay
proportionally to subtree SIZE, served by `agent_accounts_parent_idx`
(`0001_init.sql:119`). The `membership_mode = 1` hoist below keeps an
EXPLICIT channel out of either recursion, so the EXPLICIT path is
unchanged. It is still a real new cost on the hottest write path,
accepted here rather than discovered in production.

One v1 restriction follows: `mandatory_subscription` is refused on a
TREE channel (`CreateChannel` and `SetChannelPolicy` guards), because
mandatory delivery is defined over member rows (`delivery_reads.sql:14`)
and so is its D2 seeding (`SeedChannelDeliveryCursors`,
`delivery_cursors.sql:15-22`, `FROM channel_members cm`; the
newly-mandatory flip that calls it is `channels.go:794-795`), and a TREE
channel has none. Revisit when the ACL record re-cuts this surface.

**Home channels stay EXPLICIT.** They are minted ungrouped with seeded
members (`accounts.go:375-384`) and place under their agent by UI
derivation alone (leg 1); no home channel is ever TREE.

**`ReparentChannel` invariants.** Channels have no owner column —
`owner_account_id` is the post-policy operator, legal only on OWNER_ONLY
and rejected on OPEN (`channels.go:93-104`) — so "same owner" needs a
real referent. The rules, **in the order they MUST be implemented** —
the authz gate first, the shape refusals after:

- Source authz, FIRST: the caller must be a channel participant (the
  probe), and for a non-empty destination the caller's resolved owner
  must equal the destination agent's owner. Unknown channel, unknown
  agent, and non-participant all merge to `ErrNotFound`.
- **The ordering is load-bearing.** Every refusal below is
  `ErrInvalidArgument`, which is itself a channel-existence oracle: it
  tells the caller the channel exists AND is grouped / is a DM / is a
  home channel. The repo already treats exactly this discipline as
  load-bearing at the analogous gate — "This is InvalidArgument and MUST
  stay after the no-oracle owner gate above: a non-owner already
  collapsed to ErrNotFound and never reaches here, so no InvalidArgument
  signal leaks channel existence to an unauthorized caller"
  (`go/internal/store/channels.go:750-752`). The Global Constraint
  not-found/forbidden merge is only real if the merge runs first.
- **Anchor-side authority: any participant may re-anchor within the
  owner set.** Decided, not omitted. On a TREE channel the participant
  probe derives from `parent_agent_id` — the very column being mutated —
  so "participant" means anywhere in the current subtree, and a
  descendant may therefore move a channel its manager anchored, or (on
  an EXPLICIT channel) detach it to the root. This is consistent with
  DL-345's owner-set trust: the owner set is the trust boundary, and
  placement inside it is not separately privileged. No anchor-side check
  (caller-is-the-anchor, or an ancestor of it) is added. T2 pins the
  behaviour with a descendant-re-anchors-its-ancestor's-channel test so
  a later reader sees a decision, not a gap.
- Refuse `group_id IS NOT NULL` (`ErrInvalidArgument`): attach applies
  to root ungrouped channels only. This also keeps "reserved namespaces
  untouchable" structural — every live DM sits in a `__dm__` group, and
  the create-guard `isReservedDMGroupTx` (`channels.go:126-140`) is
  create-only, so without this refusal a reparent could pull a DM out of
  the reserved namespace.
- Refuse `kind != CHANNEL_KIND_CHANNEL` (kind 0, `comms.proto:290`). A
  converted DM is attachable — "a third party converts it to a named
  CHANNEL" (`comms.proto:285-286`), and conversion leaves it ungrouped
  (post-convert `GroupID` empty,
  `go/internal/store/dm_pgtest_test.go:425-427`); a live DM is not.
- Refuse a home channel (any channel referenced by an agent's
  `home_channel_id`): home channels place by derivation, never by edge.
- A TREE channel must keep an anchor: an empty destination on a TREE
  channel is `ErrInvalidArgument` (the CHECK makes it structural). An
  EXPLICIT channel may detach to the root.
- No cycle check is needed — a channel is a leaf; the test documents it.

Cross-owner note: either member of a converted DM may attach it to an
agent of its OWN owner; the other owner's accounts remain members via
the explicit arm, so an attach never subtracts access.

### 4. Migration

- **Schema**: one new numbered migration adds to `channels`:
  `parent_agent_id TEXT REFERENCES agent_accounts (account_id) ON DELETE
  RESTRICT`, nullable, NULL = root — the same shape as
  `agent_accounts.parent_agent_id`
  (`go/internal/store/migrations/0001_init.sql:103`) — plus
  `membership_mode SMALLINT NOT NULL DEFAULT 0 CHECK (membership_mode IN
  (0, 1))` — the value CHECK matching every other enum column on the
  table (`kind`, `0001_init.sql:195`; `post_policy`, `:197`) — plus two
  further CHECK constraints making the invariants structural (`group_id
  IS NULL OR parent_agent_id IS NULL`; `membership_mode = 0 OR
  parent_agent_id IS NOT NULL`), an
  index mirroring `channel_groups_parent_idx` (`0001_init.sql:174`), and
  a partial unique index `ON channels (parent_agent_id, name) WHERE
  parent_agent_id IS NOT NULL` — the agent-namespace mirror of
  `channels_group_name_key` (`0001_init.sql:210`), without which two
  same-name channels under one agent would both insert (the group index
  covers only `group_id IS NOT NULL`). The migration also creates
  `channel_subscriptions` (leg 3), shaped like `channel_members`
  (`0001_init.sql:216-222`) minus the membership meaning — **including
  its account-direction index**, the mirror of
  `channel_members_account_idx` (`0001_init.sql:224`), which the
  216-222 range stops one line short of: the composite PK serves
  channel-first lookups only, and 0001 states the reason both directions
  are indexed ("by channel (list a channel's members) and by account
  (the visible-channels query for a caller)", `:214-215`). The same
  asymmetry binds here — the three account-keyed delivery queries are
  exactly where leg 3 LEFT JOINs this table — **and, for a
  new tenant-owned table, its own `ENABLE`/`FORCE ROW LEVEL SECURITY`,
  its own `tenant_isolation` policy and its own grants.** 0001 applies
  those through a hardcoded `tenant_tables` array literal
  (`0001_init.sql:939-950`) and a `GRANT … ON ALL TABLES`
  (`0001_init.sql:927`) with no `ALTER DEFAULT PRIVILEGES` anywhere, so a
  table added by a later migration inherits NEITHER. The grant half fails
  CLOSED (permission denied under the request-path `compass_app` role,
  `0001_init.sql:881-882`); the RLS half fails OPEN — cross-tenant reads
  with a green test suite. T2 carries the exact DDL and its acceptance
  case. This is a standing hazard for every future table, not a quirk of
  this one.
- **Data**: no rewrite of `channel_groups` rows. Reserved groups
  (`__dm__`, `__coordination__`) keep working untouched. Existing
  user-created grouped channels keep `group_id` and render in the band
  leg 5 names for their group's visibility — SHARED groups in the
  shared-spaces band, OWNER groups in the root band — until a user
  attaches them to an agent via the
  new reparent RPC; home channels relocate under their agents purely by
  UI derivation (no data change). Every existing channel is
  `membership_mode = 0` by default — behaviour-preserving.
- **Wire**: purely additive — two new fields on `Channel`, two on
  `CreateChannelRequest`, one new enum, one new RPC. Nothing removed or
  renumbered, so no `reserved` statements and no breaking wire change
  (Global Constraints cites the repo's removed-vs-reserved precedent).

### 5. UI surface: one tree in the sidebar

Today `LeftSidebar` mounts `<ChannelsSection />` then `<AgentsSection />`
(`apps/ui/src/components/LeftSidebar.tsx:509-510`). `ChannelsSection`
partitions channels by group via
`channelSections(memberChannels(), store.channelGroups())`
(`LeftSidebar.tsx:312-313`; the partition function is
`apps/ui/src/comms.ts:114-117`, producing `ChannelSection { group:
ChannelGroup | undefined; channels: Channel[] }` at `comms.ts:104-107`).
`AgentsSection` renders "the existing user-organized folder tree of agents"
(`LeftSidebar.tsx:375-376`) from `agentTree(agents)` — the derivation over
`parentAgentId` at `apps/ui/src/stub-data.ts:387`, producing
`AgentTreeNode { agent: Agent; children: AgentTreeNode[] }`
(`stub-data.ts:367-370`).

After the fold the sidebar renders ONE section. Its bands are exactly
these five, and the rest of this record uses these five names for them:

- **The agent tree**, as today (`AgentLeaf` at `LeftSidebar.tsx:30`,
  `Branch` at `:94`, `Node` at `:129`), where each agent node additionally
  lists its attached channels as child rows: the agent's home channel
  (matched by `Agent.account` → `home_channel_id`) plus every channel
  whose `parentAgentId` names that agent. `AgentTreeNode` grows a
  `channels: Channel[]` member populated by the tree derivation. A home
  channel is an ordinary `CHANNEL`-kind ungrouped channel
  (`go/internal/store/accounts.go:377-380`, `Kind:
  int16(ChannelKindChannel)`), so today it lands in `channelSections`'
  trailing ungrouped section (`apps/ui/src/comms.ts:124-128`); after the
  fold the deriver claims it for the agent band and excludes it from
  every other band, so it renders exactly once. Clicking a home-channel
  child row opens the channel view; the agent row keeps its existing
  click behaviour, so the workspace stays reachable there.
- **Shared spaces** at the root: sections for SHARED groups — the badge
  branch that already exists at `LeftSidebar.tsx:341`.
- **The root band**: one section for everything the agent tree did not
  claim and the shared-spaces band does not cover — channels in OWNER
  groups and ungrouped channels alike. This band is NOT optional
  and is the common case, because OWNER is the DEFAULT group visibility
  (`CHANNEL_GROUP_VISIBILITY_OWNER = 0`, `comms.proto:226`;
  `VisibilityOwner ChannelGroupVisibility = 0`,
  `go/internal/store/types.go:64`), so every group a user creates
  without asking for SHARED lands here rather than in shared spaces.
  Read the shared-spaces filter as selecting INTO that band, never
  as excluding OWNER-grouped channels from the sidebar.
- **Direct messages**, unchanged: the existing DM subsection
  (`LeftSidebar.tsx:358-359`) stays its own band; a DM's surface is not a
  tree concern (1:1 agent DMs are already excluded from the channel list,
  `LeftSidebar.tsx:306-307`).
- **Browse**, unchanged and surviving the fold: `BrowseChannels`
  (`LeftSidebar.tsx:267`, mounted at `:366-367`) over
  `browsableChannels(store.channels())` (`:321`). It is not a band of
  the fold at all — the other four render `railChannels`, the
  `membership !== "none"` set (`apps/ui/src/comms.ts:59`), and this one
  renders the complement. It is listed because a channel the agent band
  fails to claim falls HERE, which is what T7's assertion discriminates.

**How those two bands are built, since neither is what `channelSections`
does today.** `channelSections` (`apps/ui/src/comms.ts:114-129`)
partitions by group MEMBERSHIP, not by visibility, and takes no
visibility parameter: it emits one section per group in `groups` that
has channels (`comms.ts:120-122`), then ONE trailing
`group: undefined` section holding the channels whose group is absent or
not in `groups` — `grouped.filter((c) => c.groupId === undefined ||
!groups.some((g) => g.id === c.groupId))` (`:124-126`). So an
OWNER-grouped channel lands in its own group's section today, and
"widening the trailing section" is not a filter that exists to widen.

The mechanism is to narrow the ARGUMENT rather than change the function,
and the function's existing unknown-group arm then does the widening for
free. The sidebar calls `channelSections` ONCE, passing only the SHARED
groups:

```ts
// LeftSidebar.tsx:312-313 today:
//   channelSections(memberChannels(), store.channelGroups())
const sharedGroups = () =>
  store.channelGroups().filter((g) => g.visibility === "shared");
const sections = () =>
  channelSections(unclaimedChannels(), sharedGroups());
```

`unclaimedChannels()` is `memberChannels()` (`LeftSidebar.tsx:311`)
minus what the agent band claimed, which the first T7 bullet already
owns; the change this mechanism makes is the SECOND argument.

Every SHARED group gets its own section, exactly the shared-spaces
band; every OWNER-grouped channel now names a group NOT in `groups` and
so falls through the `:124-126` predicate into the single trailing
section, alongside the genuinely ungrouped ones — exactly the root band,
as ONE section, which a post-hoc filter over the RESULT could not
produce (it would yield one section per OWNER group). The two bands are
then the leading `group !== undefined` sections and the trailing
`group === undefined` one, which is the order `channelSections` already
returns them in, so the render is a split of one list rather than two
calls. `channelSections` itself, its signature and all three of its
existing tests are UNCHANGED — the OWNER-visibility group in
`comms.test.ts:321-341` still gets its own section when it is passed in
`groups`, because the behaviour that changes is the caller's argument,
not the partition. `ChannelGroupVisibility` is `"owner" | "shared"`
(`apps/ui/src/comms-stub.ts:45`), so the filter is total and needs no
default arm. The one cost: the trailing section's header reads
`"channels"` (`LeftSidebar.tsx:340`, `section.group?.name ?? "channels"`)
— fine as the root band's label, and T7 owns it if it should read
otherwise.

**The wire gap this band depends on.** A TREE channel cannot reach the
agent band on today's contract, and the sidebar work alone cannot fix
it. `Channel.member_account_ids` (`comms.proto:243`) and
`subscriber_account_ids` (`:248`) are populated from member rows only:
`loadChannelMembers` (`go/internal/store/channels.go:883`) reads
`ChannelMembersByChannelIDs` (`:893`) and appends into
`MemberAccountIDs`, mapped to the wire at
`go/internal/comms/mapping.go:73-74`. A TREE channel has no member rows,
so both lists ship EMPTY. The UI then derives the caller's membership
entirely from them — `deriveMembership`
(`apps/ui/src/live/adapt.ts:172-176`) returns `"none"` when the caller is
in neither list, and the doc comment above it states the model: "the wire
carries no per-caller membership enum — the domain's join/subscribe model
is a UI projection over member_account_ids + subscriber_account_ids"
(`adapt.ts:161-171`). `railChannels` (`apps/ui/src/comms.ts:58-59`) keeps
only `membership !== "none"`, so every TREE channel would land in the
browse list behind the permanently disabled join button
(`LeftSidebar.tsx:289-296`, title "Joining is not wired up yet") and
never under its agent.

**Decided: the server materializes.** `loadChannelMembers` gains a
branch that, for each `membership_mode = 1` channel, fills
`MemberAccountIDs` with the derived participant set — the anchor-to-
subtree DESCENT of leg 3 in its ID-SET form, since `loadChannelMembers`
is keyed by
channel and by nothing else (`ChannelMembersByChannelIDs` takes
`$1::text[]` of channel ids, `channels.sql:73-76`) and must attribute
each row back to its channel — and
`SubscriberAccountIDs` from the `channel_subscriptions` overrides
INTERSECTED with that derived set. The existing UI projection then works
unchanged, and so does every other consumer of those two lists.

The intersection is load-bearing, not tidiness. Leg 3 leaves a stale
override row in place after a reparent-out and calls it inert — but
inert FOR DELIVERY only, because the delivery queries conjoin the
participant check. `loadChannelMembers` is a new site that no such
conjunction covers. Filled from raw override rows, a reparented-out
agent would appear in `subscriber_account_ids` and not in
`member_account_ids`, and `deriveMembership` checks subscribers FIRST
(`adapt.ts:173`) — so it would return `"subscribed"`, the top tier, and
the agent's rail would show a channel every server read gate answers
with `ErrNotFound`. It would also break the domain type's stated
invariant, "SubscriberAccountIDs is the subset of members … A subset of
MemberAccountIDs" (`go/internal/store/types.go:215-219`), and the
premise the adapter relies on: "A subscriber is by definition a member
(the server enforces 'subscribe only a current/added member')"
(`adapt.ts:169-171`).

**Cost of the materialization, on the read path.** The descent runs on
every read that loads member lists, not just the sidebar's first paint:
`loadChannelMembers` has four call sites (`channels.go:296`, `:348`,
`:829`, `:856`), so `ListChannels` pays it too. It MUST NOT become one
query per channel. The function's own contract forbids that — it
"populates each channel's member and subscriber sets with one follow-up
query over the whole id set, so member loading is O(1) round-trips
rather than one per channel" (`channels.go:879-882`) — so the derived
branch is ONE set-based recursive query over the whole mode-1 id set
(the descent seeded from every mode-1 channel's anchor at once), keeping
the round-trip count at two rather than restoring the N+1 that comment
designed out. Accepted consequence on the wire: `member_account_ids`
becomes O(subtree) per TREE channel, where today it is bounded by a
hand-managed member set and the proto documents it only as "The accounts
party to the channel" (`comms.proto:242-243`) with no size expectation.

The alternative — a per-caller membership field on the wire `Channel` —
is rejected: a larger contract change that contradicts the model
`adapt.ts:161-171` states. T4 owns the server half and pins it with the
pgtest that matters: a TREE channel's derived participants arrive in
`MemberAccountIDs` with its `channel_members` row count asserted zero in
the same test. T8 pins the client half — a TREE `WireChannel` whose
materialized `member_account_ids` CONTAINS the subtree agent derives
`"joined"` (and `"subscribed"` with the override also present). Note
what T8 canNOT assert: it is a unit test over a hand-built fixture, so
it cannot observe what the server produced. Under this shape a channel
arriving with BOTH lists genuinely empty SHOULD derive `"none"`, and
`deriveMembership` (`adapt.ts:172-176`) stays unchanged and correct in
doing so; asserting otherwise would be asserting the rejected
alternative. The regression risk lives on the server, and T4's pgtest is
where it is caught.

Dead residue removed in the same slice:

- The header button `<button type="button" class="icon-btn" title="New
  folder">` (`LeftSidebar.tsx:437`) has no click handler — nothing in the
  component wires it. T7 deletes it; a per-agent-row "new channel here"
  affordance is deferred (Open Questions).
- `app.css` retains folder-tree classes with live consumers only inside the
  agent tree: `.folder-caret` (`apps/ui/src/app.css:270`),
  `.folder-caret.collapsed` (`:280`), `.folder-badge` (`:284`),
  `.folder-children` (`:301`), plus the `.folder` wrapper class used at
  `LeftSidebar.tsx:99`. These are renamed to `tree-*` so the "folder"
  vocabulary (retired with the folder tree in PR #91) leaves the codebase.

## Alternatives considered

- **B — keep `ChannelGroup` as the user-facing namespace; UI-only merge.**
  The sidebar interleaves group sections into the agent tree by matching
  group owner to agent owner. Loses: posting boundaries do not follow the
  tree (membership still managed per-group), and the sidebar needs a
  fragile name/owner join between two unrelated hierarchies. The issue's
  stated goal — one hierarchy with tree-following boundaries — is not
  met; this is a rendering trick.
- **C — agent tree subsumes grouping entirely; drop `ChannelGroup`.**
  Delete the group table, move SHARED to a channel-level visibility field.
  Loses: OpenDM and the coordination reconcile both depend on reserved
  group rows (`go/internal/comms/dm.go:34`,
  `go/internal/comms/coordination.go:15-17`), so this forces a rewrite of
  two working subsystems; and removing `ChannelGroup` messages/RPCs is a
  breaking wire change requiring `reserved` bookkeeping. Far more blast
  radius for the same sidebar outcome. Can be revisited after this record
  ships if groups atrophy to machinery-only.
- **D — per-agent server-minted groups.** Model attachment as a reserved
  group per agent, reusing the whole existing lattice — predicates,
  per-group name uniqueness — the way the coordination reconcile already
  mints per-owner machinery groups. Loses: a group's owner is a user, not
  an agent (the `channel_groups` DDL carries `owner_user_id` and no agent
  reference at all, `go/internal/store/migrations/0001_init.sql:165-172`),
  so agent identity must
  be encoded in reserved names crowding the `__dm__`/`__coordination__`
  namespace; the sidebar still needs an agent-to-group join; and group
  visibility grants reads, not posting membership — member rows would
  still need a reconcile on every reparent, which is exactly what the
  membership ruling eliminates.
- **E — materialized-derived membership (member rows as a cache the hook
  rewrites).** Keeps every membership JOIN working unchanged, but a
  subtree reparent must rewrite member rows on every TREE channel of
  every affected ancestor chain — unbounded write amplification against
  the hook's current cost of reconciling one manager's one channel
  (`go/internal/comms/coordination.go:14-19`) — and any missed write is a
  stored-membership leak. Matt's "purely derived from the tree" is
  delivered by computing at query time, not by caching.
- **A (chosen) — channel gains an owning-agent edge plus a membership
  mode; groups demoted to machinery.** Smallest additive delta; two
  membership modes cover both ruled cases; the non-member read grant is
  the ruled owner-set default; reserved-namespace machinery untouched.

## Plan

### T1 — proto: attach edge, membership mode, `ReparentChannel` (lane: proto owner)

Add to `proto/compass/v1/comms.proto`:

- `string parent_agent_id = 11;` on `message Channel` (last used field is
  `repeated PinnedEntry pinned_entries = 10;`, `comms.proto:259`). Comment
  states the owner-set read grant and the SHARED exclusion.
- `ChannelMembershipMode membership_mode = 12;` on `message Channel`,
  with the new enum below. `ChannelKind` is not touched.
- `string parent_agent_handle = 5;` and `ChannelMembershipMode
  membership_mode = 6;` on `CreateChannelRequest` (last used field is
  `repeated string member_handles = 4;`, `comms.proto:663`), the handle
  mutually exclusive with `group_id`. Handle addressing follows the
  account convention `ReparentAgentRequest` set: "A `@handle`; the
  server resolves it to an account id; unknown → NOT_FOUND"
  (`comms.proto:714-715`).
- `rpc ReparentChannel(ReparentChannelRequest) returns
  (ReparentChannelResponse);` beside `ReparentAgent` (`comms.proto:82`).

Interfaces:

- Consumes: existing `Channel`, `CreateChannelRequest`, `ChannelChanged`.
- Produces:

```proto
enum ChannelMembershipMode {
  // Stored channel_members rows; UpdateChannelMembers manages them.
  CHANNEL_MEMBERSHIP_MODE_EXPLICIT = 0;
  // Membership derived from the agent subtree under parent_agent_id; no
  // stored member rows; UpdateChannelMembers add/remove is rejected.
  CHANNEL_MEMBERSHIP_MODE_TREE = 1;
}

message ReparentChannelRequest {
  string channel_id = 1;
  // Destination agent; empty detaches an EXPLICIT channel to the root.
  // A `@handle`; the server resolves it; unknown → NOT_FOUND.
  string new_parent_agent_handle = 2;
}

message ReparentChannelResponse {
  Channel channel = 1;
}
```

Test cycle: `buf lint` / codegen build; wire-compat check that no existing
field moved.

### T2 — store: schema migration + attach writes (lane: compass-server)

New numbered migration (shapes per Approach leg 4):

```sql
ALTER TABLE channels
    ADD COLUMN parent_agent_id TEXT
        REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    ADD COLUMN membership_mode SMALLINT NOT NULL DEFAULT 0
        CHECK (membership_mode IN (0, 1)),
    ADD CONSTRAINT channels_group_xor_agent
        CHECK (group_id IS NULL OR parent_agent_id IS NULL),
    ADD CONSTRAINT channels_tree_mode_needs_agent
        CHECK (membership_mode = 0 OR parent_agent_id IS NOT NULL);
CREATE INDEX channels_parent_agent_idx ON channels (parent_agent_id);
CREATE UNIQUE INDEX channels_agent_name_key
    ON channels (parent_agent_id, name) WHERE parent_agent_id IS NOT NULL;
CREATE TABLE channel_subscriptions (
    channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    subscribed BOOLEAN NOT NULL DEFAULT FALSE,
    tenant_id  TEXT NOT NULL
        DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (channel_id, account_id)
);
-- Both directions indexed, the mirror of channel_members_account_idx
-- (0001_init.sql:224) and for 0001's stated reason (:214-215): the composite
-- PK serves channel-first lookups, and the three account-keyed delivery
-- queries drive account-first.
CREATE INDEX channel_subscriptions_account_idx
    ON channel_subscriptions (account_id);
-- The tenant_id column is only HALF the isolation mechanism. 0001's ENABLE
-- + FORCE + policy loop runs over a HARDCODED array literal
-- (0001_init.sql:939-950), and its GRANT is `ON ALL TABLES` at grant time
-- (0001_init.sql:927) with no ALTER DEFAULT PRIVILEGES anywhere, so a new
-- table inherits NEITHER. The grant half fails CLOSED (permission denied
-- under SET LOCAL ROLE compass_app, caught by the first pgtest); the RLS
-- half fails OPEN (cross-tenant reads, a green suite). Both halves are
-- therefore restated here explicitly, the policy in 0001's frozen T2 form
-- (0001_init.sql:955-961): scalar-subquery GUC read, non-empty guard,
-- tenant_id equality, as both USING and WITH CHECK.
ALTER TABLE channel_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON channel_subscriptions
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_subscriptions
    TO compass_app, compass_system;
```

0001's hardcoded `tenant_tables` array is a standing hazard for every
future table, not just this one: any migration that adds a tenant-owned
table must restate RLS, the policy and the grants itself, because
nothing in the schema does it automatically and the omission is silent.

Store writes:

- `CreateChannel` accepts `ParentAgentID` + `MembershipMode`; rejects
  both `GroupID` and `ParentAgentID` set, TREE without an anchor, TREE
  with `mandatory_subscription`, and TREE with an `OWNER_ONLY` post
  policy (`ErrInvalidArgument` each — the last one per the leg-3
  decision, which binds both writers, and which the existing coherence
  check at `channels.go:170` cannot catch because a TREE create writes
  no member rows); authorizes attach — the caller must be the agent's
  owner or an agent with the same owner, unknown agent merged to
  `ErrNotFound` (the gate shape at
  `go/internal/store/channels.go:114-117`). EXPLICIT attach keeps
  today's member augmentation (`channels.go:76-79`); a TREE create still
  runs `expandOwnerMembership` (`channels.go:160`) for the authz and the
  returned list, but skips the `EnsureChannelMember` loop over it
  (`channels.go:174-184`) and returns the derived participant set as
  `MemberAccountIDs` instead (leg 3) — which after T4's hop (v) it gets
  from the post-commit `s.getChannel` re-read rather than from a
  hand-written literal. T4 owns that swap because it owns the two
  columns the literal would otherwise drop; T2 owns only the two new
  input fields, and MUST NOT set them on the literal as a shortcut,
  because a fifth `Channel` construction site is the defect, not the
  missing assignment.
- `membership_mode` is immutable after create (leg 3 invariant): no
  store write may change it, pinned by a test.
- `Store.ReparentChannel` enforcing every invariant in Approach leg 3
  **in the order leg 3 lists them**: the participant-plus-same-owner
  authz gate with the `ErrNotFound` merge FIRST, then the
  grouped-channel, kind, home-channel and TREE-keeps-anchor refusals.
  The order is load-bearing, not cosmetic — see leg 3.

Interfaces:

- Consumes: `store.NewChannel` (`go/internal/store/inputs.go:52`; gains
  `ParentAgentID AccountID` and `MembershipMode ChannelMembershipMode`),
  new sibling of `requireGroupCreateAuthz` (`authz.go:89`):
  `requireAgentAttachAuthz(ctx context.Context, q db.DBTX, actor
  AccountID, agentID AccountID) error`.
- Produces: `func (s *Store) ReparentChannel(ctx context.Context, actor
  AccountID, channelID ChannelID, newParentAgentID AccountID) (Channel,
  error)`; `ChannelMembershipMode` domain type in `types.go`; the
  migration file.

Test cycle: pgtest + **`sql-migration-gate`** (squawk + sqruff over
`go/internal/store/migrations/*.sql`,
`tools/sql-migration-gate/index.ts:38`) — this is the FIRST migration
after the bootstrap, so `.squawk.toml`'s own revisit-note is now live:
"They stay OFF only for this bootstrap posture — a future incremental
migration against live data would want them back (revisit this list when
the first post-live migration lands)" (`.squawk.toml:10-13`). Confirmed
for THIS migration, with the reason per exclusion: `channels` is small
and the added column carries a constant default (no table rewrite on PG
11+), the two index builds are non-`CONCURRENT` but run pre-live, and
`prefer-bigint-over-smallint` (`.squawk.toml:44`) still holds because
`membership_mode` is a two-value CHECK-able enum, the same shape as
`channels.post_policy` (`0001_init.sql:197`,
`SMALLINT NOT NULL DEFAULT 0 CHECK (post_policy IN (0, 1))`). The
exclusions are re-confirmed here, not narrowed; the first migration
against genuinely live data must narrow them. sqruff's capitalisation
rules stay ON and lint this DDL block.

pgtest cases: create-under-agent happy path in both modes; cross-owner
attach → `ErrNotFound`; both-parents-set, TREE-without-anchor,
grouped-channel reparent, DM-kind reparent, home-channel reparent, and
TREE-detach-to-root → `ErrInvalidArgument`; **a non-participant
reparenting a grouped channel gets `ErrNotFound`, not
`ErrInvalidArgument`** (the M1 ordering, pinned by a test rather than by
bullet order); **a descendant re-anchors its ancestor's channel and
succeeds** (the M4 permissive decision, pinned so a later reader cannot
read it as an omission); **a second tenant reads zero
`channel_subscriptions` rows written under the first tenant's GUC** (the
RLS policy, which fails OPEN if the DDL is missed and so cannot be left
to the grant's permission-denied); **an `OWNER_ONLY` post policy on a
TREE create → `ErrInvalidArgument`** (the N2 both-writers refusal, the
create half of the pair T5 asserts on the update half);
**`mandatory_subscription` on a TREE create → `ErrInvalidArgument`**
(the create half of its own pair, whose update half T5 asserts as the
mandatory flip — the refusal binds both writers exactly as the
`OWNER_ONLY` one does, so its coverage is symmetric too); **no store path
changes `membership_mode` after create** (the leg-3 immutability
invariant, asserted rather than assumed absent); duplicate name under
one agent → `ErrConflict`; converted-DM attach by each of its two
owners; a channel is a leaf, so no cycle is possible (test documents
it).

### T3 — store: the participant probe (lane: compass-server)

Add `ChannelParticipant` — the explicit arm OR the tree arm of Approach
leg 3 — BESIDE the existing `ChannelMemberExists`
(`channels.sql:36-37`), and rebind the wrappers `requireChannelMember` /
`isChannelMember` (`go/internal/store/authz.go:23` and `:51`) onto it.
`ChannelMemberExists` SURVIVES: two callers genuinely mean
stored-member-row semantics, not participation, and must not inherit the
derived arm (Approach leg 3). Give `TopicChannelMemberExists`
(`authz.sql:8-10`) the same tree arm with the channel resolved through
`topics.channel_id`.

Wrapper signatures and the not-found/forbidden merge (`authz.go:31-35`)
are unchanged, so every wrapper caller inherits derived membership
without an edit: `AppendMessage` (`messages.go:59`),
`UpdateChannelMembers` (`channels.go:387`), `SetChannelPolicy`
(`channels.go:704`), `requireBoardMutator`
(`channel_pins.go:179-180`), `IsChannelMember` (`authz.go:45`),
`IsTopicChannelMember` (`authz.go:70`), and `ListTopics`
(`go/internal/store/topics.go:23`, which calls the unexported
`isChannelMember` directly, not the exported stream filter).

Interfaces:

- Consumes: `channels.membership_mode`, `channels.parent_agent_id`,
  `agent_accounts.parent_agent_id`, and the
  `channels_tree_mode_needs_agent` CHECK that makes a mode-1 row's
  anchor non-NULL — without it the owner-arm scalar subquery compares
  against NULL for every mode-1 row (all T2).
- Produces: sqlc queries `ChannelParticipant :one` and
  `TopicChannelParticipant :one` (additive — `ChannelMemberExists` and
  `TopicChannelMemberExists` both stay generated); regenerated `db`
  package; `requireChannelMember(ctx context.Context, q db.DBTX, actor
  AccountID, channelID ChannelID) error` unchanged.

Test cycle: pgtest — subtree agent posts into an ancestor-anchored TREE
channel; a subtree agent calls `ListTopics` on an ancestor-anchored TREE
channel and gets its topics rather than `ErrNotFound` (the `ListTopics`
acceptance case); an agent outside the subtree and a foreign user →
`ErrNotFound`; the anchor's owner posts; after `ReparentAgent` moves an
agent out of the subtree its next post → `ErrNotFound` with
`channel_members` row count asserted unchanged (no reconcile ran);
`hasGenuineAdd` still reports a genuine add for an account that is a
DERIVED participant but has no member row (the member-row semantics it
needs, proving the probe swap did not leak into it); the full existing
suite stays green (EXPLICIT behaviour identical).

### T4 — store: visibility predicates + read paths (lane: compass-server)

- Extend all three channel predicate copies (`ListChannels`,
  `ChannelVisibleTo`, `ChannelsByNameForViewer`;
  `channels.sql:129`, `:159`, `:188`) with the `viewer` CTE and the
  owner-set disjunct of Approach leg 2, textually identical per
  `channels.sql:10-12`.
- **The two new columns are carried from the row to the wire.** T2 adds
  `channels.parent_agent_id` and `channels.membership_mode`, and T1 adds
  the wire fields `Channel.parent_agent_id = 11` /
  `membership_mode = 12`, but nothing between them moves either value:
  the shared channel projection is a fixed seven-column SELECT list, and
  `channelFromRow`'s own doc comment names that shape — "the shared
  seven-column channel projection every channel read selects"
  (`go/internal/store/channels.go:863`). This task owns the carry rather
  than T2, because T2 owns the schema and the write paths while the
  carry is entirely a READ-path edit in the `db` package this task
  already regenerates, on queries it mostly already rewrites. Note the
  two triples are NOT the same three queries, and both are complete:
  the PREDICATE copies of the bullet above are `ListChannels`,
  `ChannelVisibleTo` and `ChannelsByNameForViewer`, while the
  PROJECTION copies are `GetChannel`, `ListChannels` and
  `ChannelsByNameForViewer`. They overlap in two. `GetChannel`
  (`channels.sql:68-71`) carries no visibility predicate and so is
  untouched by that bullet, and `ChannelVisibleTo` projects a bare
  `SELECT EXISTS (…)` with no column list at all
  (`channels.sql:173-186`) and so is untouched by this one. Five hops,
  all five required. **(i) The projection**
  gains both columns in all three PROJECTION copies — `GetChannel`
  (`channels.sql:69`), `ListChannels` (`:143`) and
  `ChannelsByNameForViewer` (`:202`), each of which today reads
  `SELECT id, name, COALESCE(group_id, '') AS group_id, kind,
  post_policy,` over `COALESCE(owner_account_id, '') AS
  owner_account_id, mandatory_subscription`. `parent_agent_id` is
  nullable, so it projects as `COALESCE(parent_agent_id, '') AS
  parent_agent_id`, the shape the list already uses for both of its
  other nullable columns; `membership_mode` is `NOT NULL` and projects
  bare. The identical-copies rule (`channels.sql:10-12`) already binds
  the three to change in one commit. **(ii) `channelFromRow`**
  (`channels.go:865`) takes the two new values, and every call site
  passes them: `ListChannels` (`channels.go:294`),
  `ChannelByNameForViewer` (`:346`), `getChannel` (`:828`) and
  `scanChannels` (`:851`, whose manual `rows.Scan` at `:848` grows the
  two scan destinations as well). Those four were enumerated by
  `git grep -nE 'channelFromRow'` over `go/`, not by recall, and its doc
  comment's "seven-column" wording (`:863`) becomes nine. **(iii)
  `store.Channel`** (`go/internal/store/types.go:204-225`) gains
  `ParentAgentID AccountID` — empty = tree root, the same
  empty-means-root encoding `AgentAccount.ParentAgentID` already uses
  ("the agent's parent in the agent tree; empty = root",
  `types.go:181-183`) — and `MembershipMode ChannelMembershipMode`, the
  domain type T2 produces. **(iv) `channelToWire`**
  (`go/internal/comms/mapping.go:67-79`) maps both, beside the nine
  fields it sets today. **(v) `CreateChannel`'s returned literal**
  (`channels.go:206-213`) — because `channelFromRow`'s call sites are
  NOT the same set as the places a `Channel` is BUILT, and hops (i)
  through (iv) reach only the read paths. `CreateChannel` hand-writes
  its return value after commit rather than re-reading the row:
  `return Channel{ID: ChannelID(id), Name: c.Name, GroupID: c.GroupID,
  Kind: c.Kind, MemberAccountIDs: members, Policy: c.Policy}, nil`. It
  never calls `channelFromRow`, so every field it does not name stays at
  the zero value however correctly the four hops above are implemented.
  **The fix is structural, not another assignment**: `CreateChannel`
  returns `s.getChannel(ctx, ChannelID(id))` after the commit, which is
  exactly what its three sibling writers already do —
  `UpdateChannelMembers` (`channels.go:471`), `SetChannelPolicy`
  (`:803`) and, at the comms layer, `UpdatePinnedBoard`
  (`go/internal/comms/comms.go:625`, whose own comment states the reason:
  "Re-read the channel so ChannelChanged and the response carry the
  current member/policy projection alongside the updated board"). That
  collapses the construction sites to ONE, so no later column addition
  can miss this path again, and it is the same one-line pattern rather
  than a new one. It also makes the create response's
  `MemberAccountIDs` literally the projection a later `ListChannels`
  reports, which is the invariant leg 3 already claims ("a TREE
  `CreateChannel` and a later `ListChannels` report the same member list
  for the same channel", :274-275) and which the literal today only
  approximates by reconstructing it — leg 3's citation of
  `channels.go:211` for that carry-back becomes the re-read, and the
  expansion it describes is still run, for the authz. The re-read is a
  pool read after commit, the posture `GetChannel`'s doc comment already
  states for the sibling path ("It is a pool read (post-commit), not a
  tx read", `channels.go:807-808`). **The re-read is fallible where the
  literal was not**: a failure between `tx.Commit` (`channels.go:202`)
  and the read — a pool blip, or a client-cancelled ctx — returns an
  error for a channel that IS durably created, and because
  `CreateChannel` is not idempotent the natural retry then conflicts
  (`ErrConflict`, `channels.go:152`) on T2's own new
  `channels_agent_name_key` (:1038-1039). **Accepted, not softened**,
  because both store siblings propagate their post-commit re-read error
  identically (`channels.go:471-473`, `:803`) — only the coordination
  EVENT path is best-effort, and only because there the read is not the
  return value ("A read failure is logged and skipped, never
  propagated", `coordination.go:154-155`) — and a literal-shaped
  fallback on the error arm would reintroduce the fifth construction
  site this hop exists to remove, handing T2 back the shortcut the
  bullet above forbids. The write is durable and the channel appears on
  the next `ListChannels`. **The enumeration behind
  this hop, since the four-call-site list above is what hid it:** every
  `Channel` construction in the store package was swept with
  `git grep -nE 'return Channel\{' c7c73135 -- go/internal/store/` — 49
  hits, of which 47 are zero-value `Channel{}` error returns and exactly
  TWO populate fields, `channelFromRow` (`:866`) and this one (`:206`).
  The only non-`return` composite literal is `[]Channel{channelFromRow(…)}`
  (`:828`), which wraps hop (ii) and needs nothing of its own. Outside
  the package there are none: `git grep -nE 'store\.Channel\{' c7c73135
  -- go/` returns nothing (rc=1; positive control in the same run,
  `store.Channel` in `mapping.go` = 18 hits), so every `Channel` that
  reaches a caller or an event originates at one of those two sites.
  With hop (v) there are no further bypass sites.

  Miss hops (i) through (iv) and a TREE channel reaches the
  client with `parentAgentId` unset, T7's deriver — which slots channels
  by `parentAgentId` — sends every attached channel down its
  dangling-parent path, and the agent band this record exists to build
  renders empty. Miss hop (v) and the same thing happens on exactly one
  path — the one where the user is watching for the channel to appear.
  `Comms.CreateChannel` uses the store's returned value TWICE:
  `CreateChannelResponse.channel` is `channelToWire(ch)`
  (`go/internal/comms/comms.go:247`), a wrong value on the wire rather
  than a missing one; and `c.publishChannelChanged(ch, nil)` (`:246`)
  maps that same literal into a `ChannelChanged` (`mapping.go:484-493`),
  which the UI reducer upserts straight into state (`case
  "channelChanged": return { ...state, channels: upsertById(…) }`,
  `apps/ui/src/live/comms-state.ts:219-220`, adapted at
  `apps/ui/src/live/stream.ts:214`). So a channel just created WITH an
  anchor enters the sidebar anchor-less and renders in the root band
  instead of under its agent, self-healing only on the next
  `ListChannels` or a later event from a path that re-reads. The other
  four `publishChannelChanged` callers are unaffected and that is what
  makes the create path singular: `UpdateChannelMembers` (`:274`),
  `SetChannelPolicy` (`:575`) and `UpdatePinnedBoard` (`:625`) all
  re-read through `getChannel`, the coordination reconcile re-reads via
  `c.store.GetChannel` (`coordination.go:163`), and `dm.go:75` forwards
  a value its caller already read. This is a DIFFERENT gap from the H3
  materialization
  below: that one fills the two member LISTS, these are two other fields
  on the same message, and neither fix implies the other.
- Swap the direct member JOINs of the message/topic reads for the
  participant shape: `GetPageCursorSeq` (`messages.sql:64`),
  `ListMessages` (`:71`), `SearchMessages` (`:81`), `FindAskMessage`
  (`:92`), `UpdateMessageBlocksAsAuthor` (`:53`),
  `ResolveTopicForUpdate` (`topics.sql:16`).
- **The wire's member lists gain a derived branch (H3).**
  `Channel.member_account_ids` (`comms.proto:243`) and
  `subscriber_account_ids` (`:248`) are populated from member rows only
  — `loadChannelMembers` (`go/internal/store/channels.go:883`) calls
  `ChannelMembersByChannelIDs` (`:893`) and appends into
  `MemberAccountIDs`, mapped to the wire at
  `go/internal/comms/mapping.go:73-74`. A TREE channel therefore ships
  with BOTH lists empty, and the UI derives the caller's membership
  purely from them: `deriveMembership`
  (`apps/ui/src/live/adapt.ts:172-176`) returns `"none"` when the caller
  is in neither, and `railChannels` (`apps/ui/src/comms.ts:58-59`)
  filters `membership !== "none"`, so the channel lands in the browse
  list behind the permanently disabled join button
  (`LeftSidebar.tsx:289-296`) and never in the agent tree. Chosen fix:
  the SERVER materializes the derived participant set into
  `MemberAccountIDs` at read time — `loadChannelMembers` gains a branch
  that takes the ANCHOR-TO-SUBTREE DESCENT of Approach leg 3, not the
  probe's ascent: the function is keyed by channel and by nothing else
  (`ChannelMembersByChannelIDs` takes `$1::text[]` of channel ids,
  `channels.sql:73-76`), so it has no actor to seed an ascent from. The
  branch is ONE set-based recursive query over the whole mode-1 id set,
  NOT one per channel: the function's contract is "one follow-up query
  over the whole id set, so member loading is O(1) round-trips rather
  than one per channel" (`channels.go:879-882`), and a per-channel
  descent would restore exactly the N+1 that comment designed out. It
  takes the descent's ID-SET form (leg 3), which projects
  `(channel_id, account_id)` PAIRS rather than bare account ids,
  because this function attributes every row it consumes back to a
  channel through `idx := byID[ChannelID(m.ChannelID)]`
  (`channels.go:895-902`) and a union of several anchors' subtrees
  projecting bare accounts cannot be attributed. The two channel-keyed
  delivery queries T5 rewrites use the SINGLE-CHANNEL form instead,
  each being called with one `channel_id` (`delivery_reads.sql:13`,
  `:22`).
  `SubscriberAccountIDs` is filled from the `channel_subscriptions`
  override rows INTERSECTED with that derived set, so a stale override
  left by a reparent-out — which leg 3 keeps deliberately, inert for
  DELIVERY only — cannot escape onto the wire and make
  `deriveMembership` return `"subscribed"` (it checks subscribers first,
  `adapt.ts:173`) for an agent every read gate refuses. The intersection
  also holds the domain invariant "SubscriberAccountIDs is the subset of
  members" (`go/internal/store/types.go:215-219`). Cost: the descent
  runs on all four `loadChannelMembers` call sites (`channels.go:296`,
  `:348`, `:829`, `:856`), so every `ListChannels` pays it, and
  `member_account_ids` becomes O(subtree) per TREE channel on the wire
  where the proto states no size expectation ("The accounts party to the
  channel", `comms.proto:242-243`). Both accepted. The alternative — a
  per-caller membership
  field on the wire `Channel` — is rejected: it is a larger contract
  change and contradicts the stated model at `adapt.ts:161-171` ("the
  wire carries no per-caller membership enum — the domain's
  join/subscribe model is a UI projection over member_account_ids +
  subscriber_account_ids"). Materializing keeps every existing consumer
  of those lists working with no UI contract change.
- Out of scope, stated: `SharesVisibleChannel`
  (`presence_reads.sql:17-18`) and the visible-accounts arm
  (`accounts.sql:133-134`) stay member-row-based (leg 3 degradation
  note).

Interfaces:

- Consumes: T2's two columns and its `ChannelMembershipMode` domain
  type; T3's participant shape.
- Produces: updated sqlc queries + regenerated
  `go/internal/store/db/*.sql.go`; the two new columns in all three
  channel projections, in `channelFromRow` (`channels.go:865`) and its
  four call sites, as `Channel.ParentAgentID` /
  `Channel.MembershipMode` (`types.go:204-225`) and in `channelToWire`
  (`mapping.go:67-79`); `CreateChannel` returning
  `s.getChannel(ctx, ChannelID(id))` in place of its hand-written
  literal (`channels.go:206-213`), so `channelFromRow` becomes the sole
  `Channel` construction point; a derived branch in `loadChannelMembers`
  (`channels.go:883`); predicate parity test.

Test cycle: pgtest — owner and a same-owner sibling agent see an attached
channel in `ListChannels`; another user does not; `ChannelVisibleTo`
agrees with `ListChannels` on every case (the stream-edge parity the
copies rule protects); a subtree agent reads `ListMessages` on an
ancestor TREE channel; an owner-set non-member sees the channel row but
reads no history; and — the H3 assertion, the one that catches a server
half regressing — a TREE channel returned by `ListChannels` carries
every derived participant in `MemberAccountIDs` despite having zero
`channel_members` rows, with the row count asserted zero in the same
test so the materialization cannot be mistaken for a seeding
regression; and a reparented-out agent holding a live
`channel_subscriptions` override appears in NEITHER list (the
intersection).

Three further assertions, all of which FAIL under this record as written
before this task's carry, create-path re-read and set-based projection
land, and so are the acceptance for them:

- A channel created with an anchor comes back from `ListChannels` — and
  from `ChannelByNameForViewer` and `GetChannel`, the other two
  projection copies — with
  `ParentAgentID` equal to that anchor and `MembershipMode` equal to the
  mode it was created with, not the zero value. Nothing before this task
  puts either column in a SELECT list, so this fails on today's
  seven-column projection however correct T1 and T2 are. Covering all
  three copies is what the identical-copies rule
  (`channels.sql:10-12`) asks for: a fix applied to one or two copies
  still goes red, and `GetChannel` is the copy behind `s.getChannel`,
  the return path for `SetChannelPolicy` (`channels.go:803`),
  `UpdateChannelMembers` (`:471`), `GetChannel` (`:815-816`), the
  coordination reconcile's post-commit event read
  (`coordination.go:163`) and — after hop (v) — `CreateChannel` itself.
- The `Channel` that `CreateChannel` ITSELF returns — not a subsequent
  read — carries the anchor and the mode. This is the assertion for hop
  (v) specifically and nothing else in the record catches it: the
  assertion above goes green the moment the three projections and
  `channelFromRow` land, while a `CreateChannel` still returning its
  hand-written literal (`channels.go:206-213`) hands back
  `ParentAgentID` empty and `MembershipMode` at zero for a channel just
  created WITH an anchor. Assert it at the store boundary on the
  returned value, and — because the same value feeds both the RPC
  response and the event — at the comms boundary too: a
  `CreateChannel` RPC on a TREE channel returns a
  `CreateChannelResponse.channel` with `parent_agent_id` set, and emits
  exactly one `ChannelChanged` whose `channel` carries the same value
  (the fan-out T6 owns, asserted here on the payload rather than on the
  delivery). Both halves fail today, and they fail with a WRONG value on
  the wire rather than an absent one, which is why the read-path
  assertion cannot stand in for them.
- The materialization runs over TWO TREE channels anchored at DIFFERENT
  agents in one `ListChannels`, and each channel's `MemberAccountIDs`
  holds exactly its own anchor's subtree. An id-set descent projecting
  bare `account_id` merges the two subtrees and fails this, which makes
  it the assertion for the attributable `(channel_id, account_id)`
  projection specifically, rather than for the descent's direction.

### T5 — store: subscriptions + delivery for TREE channels (lane: compass-server)

- `UpdateChannelMembers` on a TREE channel: add/remove →
  `ErrInvalidArgument`; a `Subscribed` toggle (`MemberUpdate`,
  `go/internal/store/inputs.go:69-74`) for a derived member upserts a
  `channel_subscriptions` row (mirror of `UpsertChannelMember`,
  `channels.sql:25-28`) and seeds the D2 delivery cursor on subscribe
  (the seed-at-subscribe discipline, `channels.go:642`); a toggle for an
  account outside the derived set → `ErrNotFound`.
- **The five delivery-side queries change their DRIVING RELATION, not a
  WHERE disjunct.** Every one of them reads `FROM channel_members cm`
  (`delivery_reads.sql:10` `SubscribedAgents`, `:18-24`
  `ChannelAgentMembers`, `:41` `SweepChannels`;
  `delivery_cursors.sql:83` `UndeliveredMessages`, `:102` `InSweepSet`),
  so a TREE channel — which has zero member rows — yields the empty set
  before any predicate runs. Each query's `channel_members` scan is
  replaced by a `participants` CTE: the UNION of the stored member rows
  and the derived participant set, with `channel_subscriptions` LEFT
  JOINed to supply `COALESCE(cs.subscribed, FALSE) AS subscribed` for
  the derived arm — coalesced, not raw, so the arm's semantics do not
  depend on the TREE-only refusals that keep the disjunct's other two
  terms FALSE (leg 3). The existing subscription disjunct
  (`delivery_reads.sql:14`, `:45`; `delivery_cursors.sql:91`, `:107`)
  then reads `subscribed` off that CTE unchanged.
- **Which of leg 3's two walks the derived arm takes is per site, set by
  the site's key.** The two channel-keyed queries take the
  anchor-to-subtree DESCENT — `SubscribedAgents`
  (`WHERE cm.channel_id = $1`, `delivery_reads.sql:13`) and
  `ChannelAgentMembers` (`:22`) — because their only account parameter
  is the author to EXCLUDE (`cm.account_id <> $2`, `:15`, `:23`) and an
  ascent has nothing to seed from. The three account-keyed queries take
  the actor-to-root ASCENT: `SweepChannels`
  (`WHERE cm.account_id = $1`, `:44`), `UndeliveredMessages`
  (`delivery_cursors.sql:90`) and `InSweepSet` (`:105-106`, which keys
  on both but is a single-actor probe, so the depth-bounded ascent is
  the cheaper walk). Getting this backwards on any site yields the
  wrong question's answer, not a slow one. Both descent sites take the
  SINGLE-CHANNEL form of leg 3's descent — each is invoked with one
  `channel_id` and needs no attribution, so the bare-`account_id`
  projection is right for them. The ID-SET form that projects
  `(channel_id, account_id)` pairs belongs to `loadChannelMembers` (T4)
  alone, which is the only site handed a whole id set at once.
- `ChannelAgentMembers`
  has no subscription predicate at all — it needs only the union — and
  it is the site that makes @mentions work: `resolveMentioned`
  (`go/internal/delivery/dispatch.go:278`) reads it for both reserved
  expansion (`@everyone`/`@agents`) and the per-handle membership check,
  and `dispatch.go:272-273` documents that "a resolved agent that is not
  a channel member is also a no-op". Without this change every mention
  in a TREE channel is silently dropped.
- **Cost, stated.** This puts a recursive CTE on the delivery fan-out
  path, evaluated per post rather than per turn, where today's shape is
  a `channel_members` index scan on `(channel_id, account_id)`
  (`0001_init.sql:216-222`). The ascent is the actor's ancestor chain
  (depth of the agent tree, small); the descent is the anchor's whole
  subtree, so the two channel-keyed fan-out sites pay by subtree SIZE,
  served by `agent_accounts_parent_idx` (`0001_init.sql:119`). The same
  `membership_mode = 1`
  hoist as the probe (leg 3) keeps an EXPLICIT channel out of either
  recursion, so the EXPLICIT hot path is unchanged. It is
  nonetheless a real new cost on the hottest write path and is accepted
  here, not hidden.
- `SetChannelPolicy` refuses a `mandatory_subscription` flip on a TREE
  channel (`ErrInvalidArgument`), pairing the `CreateChannel` guard (T2).
- `SetChannelPolicy` also refuses `OWNER_ONLY` on a TREE channel
  (`ErrInvalidArgument`), per the leg-3 decision on the owner-is-member
  coherence check (`channels.go:757-775`).

Interfaces:

- Consumes: `channel_subscriptions` (T2), participant shape (T3).
- Produces: `UpsertChannelSubscription :exec`; the `participants` CTE
  rewritten into all five delivery queries, each with the walk its key
  requires; updated `MemberUpdate` doc contract.

Test cycle: pgtest — subscribe toggle on a TREE channel puts the agent in
`SubscribedAgents`; the same agent appears in `SweepChannels`,
`UndeliveredMessages` and `InSweepSet` for that channel (the four sites
that gate delivery, each asserted, since a WHERE-only change would pass
none of them, and a descent-for-ascent swap would pass the wrong two);
an `@handle` mention of a subtree agent in a TREE channel
resolves through `ChannelAgentMembers` and steers that agent, and
`@agents` expands to the derived agent set — the mention acceptance case,
and the assertion that the channel-keyed descent landed, since no ascent
can produce this row; a derived participant who never toggled a
subscription is absent from `SubscribedAgents` (the `COALESCE` default);
a reparent-out makes the same override row inert (no delivery, no
membership) with no cleanup write; add/remove on TREE →
`ErrInvalidArgument`; mandatory flip on TREE → `ErrInvalidArgument`;
OWNER_ONLY flip on TREE → `ErrInvalidArgument` (the update half of the
both-writers refusal T2 asserts at create).

### T6 — comms service: RPC edge + events (lane: compass-server)

Wire `ReparentChannel` into `Comms` beside `ReparentAgent`
(`go/internal/comms/comms.go:285`), resolving `new_parent_agent_handle`
at the edge exactly as `ReparentAgent` resolves handles; pass
`parent_agent_handle` + `membership_mode` through `CreateChannel`
(`comms.go:228`); emit `ChannelChanged` post-commit. No coordination-hook
change — the hook stays agent-edge-only (Global Constraints).

Interfaces:

- Consumes: `store.ReparentChannel` (T2), predicate reads (T4).
- Produces: `func (c *Comms) ReparentChannel(ctx context.Context, req
  *connect.Request[compassv1.ReparentChannelRequest])
  (*connect.Response[compassv1.ReparentChannelResponse], error)`;
  `ChannelChanged` fan-out on attach/move.

Test cycle: comms pgtest — reparent emits exactly one `ChannelChanged`,
delivered only to accounts the predicate admits; a subtree agent's post
into a TREE channel succeeds end to end; a non-participant post still →
`ErrNotFound`.

### T7 — UI: one sidebar tree (lane: compass-ui)

- Extend `AgentTreeNode` (`apps/ui/src/stub-data.ts:367-370`) with
  `channels: Channel[]`; extend `agentTree` itself
  (`stub-data.ts:387`) — NOT a wrapping deriver in `comms.ts`, so the
  stable-input-order and dangling-parent contracts documented at
  `stub-data.ts:372-386` stay stated once where they are enforced — to
  slot each channel under its `parentAgentId` agent and each home
  channel under its agent via `home_channel_id`. A channel claimed by
  the agent band (attached or home) is excluded from the shared-spaces
  and root bands — no double render (leg 5). The agent band
  admits a TREE channel only because T4 materializes its derived
  participant set into `member_account_ids` (H3); without that server
  half the band renders nothing, whatever this deriver does.
- Replace the `<ChannelsSection /><AgentsSection />` pair
  (`apps/ui/src/components/LeftSidebar.tsx:509-510`) with one section:
  agent tree band (channels as child rows under `AgentLeaf` at
  `LeftSidebar.tsx:30`, `Branch` at `:94`, `Node` at `:129`),
  shared-spaces band and root band from ONE `channelSections` call
  (`apps/ui/src/comms.ts:114-129`) passed only the SHARED groups, per
  the leg-5 mechanism: its per-group sections are the shared-spaces
  band and its trailing `group: undefined` section is the root band,
  which is where every OWNER-grouped channel now falls via the existing
  unknown-group arm (`comms.ts:124-126`). The caller at
  `LeftSidebar.tsx:312-313` changes its `groups` argument;
  `channelSections` itself does not change. DM band
  unchanged
  (`LeftSidebar.tsx:358-359`; 1:1 agent DM exclusion at `:306-307`), and
  the browse band untouched (`LeftSidebar.tsx:366-367`).
- Dead residue: delete the handler-less `title="New folder"` button
  (`LeftSidebar.tsx:437`); rename `.folder`, `.folder-caret`,
  `.folder-caret.collapsed`, `.folder-badge`, `.folder-children`
  (`apps/ui/src/app.css:270,280,284,301`; wrapper used at
  `LeftSidebar.tsx:99`) to `tree-*` and update their consumers.

Interfaces:

- Consumes: `Channel.parentAgentId?: string` and
  `Channel.membershipMode?: "explicit" | "tree"` added to the UI
  `Channel` (`apps/ui/src/comms-stub.ts:98-105`);
  `AgentAccount.homeChannelId`; the `agentTree` contract.
- Produces: `AgentTreeNode { agent: Agent; channels: Channel[];
  children: AgentTreeNode[] }`; a single sidebar section component
  whose `channelSections` argument is the SHARED-group subset;
  renamed CSS classes.

Test cycle: `bun test` on the tree deriver (channel slotting,
home-channel placement and single-render, dangling `parentAgentId`
promotes to the root band); a case that a TREE channel reaching the
deriver with a non-`none` membership lands in the agent band rather
than the browse band; a case that an OWNER-grouped channel lands in the
trailing root-band section rather than its own section, which is the
assertion for the SHARED-only `groups` argument specifically and fails
if the caller keeps passing every group; existing `board.test.ts` /
`adapt.test.ts` stay green, and so does `comms.test.ts` — its three
`channelSections` cases (`comms.test.ts:294-354`) are UNCHANGED by
design, including the OWNER-visibility group that still gets its own
section when passed in `groups` (`:321-341`), because the behaviour
that moves is this caller's argument and not the partition. A red
`comms.test.ts` means the mechanism was implemented in
`channelSections` instead of at its call site.
`apps/ui/src/components/LeftSidebar.test.tsx` does NOT stay green, and
is rewritten here BY DESIGN — unlike `comms.test.ts`, a red one is
expected, not a sign the mechanism landed in the wrong place: its
two-section contract (`:91-134` "both sections collapse and expand
independently", driving `findToggle(container, "Channels")` and
`findToggle(container, "Agent workspaces")` and asserting each
collapses independently, plus the `"Agent workspaces"` toggle assertion
at `:203`) is what leg 5's ONE section reverses, and its New-folder pin
(`:411-418`, "the keep-native new-folder button still carries its
native title (sweep boundary held)", asserting a `button.icon-btn` with
`title === "New folder"` exists) is what Open Question 4's delete
reverses. `LeftSidebar.live.test.tsx` is NOT affected: it asserts only
`.tree-agent` / `.tree-empty` rows inside the agent band (`:103`,
`:110`, `:126`, `:169`) and never locates a section header.

### T8 — live adapter + fixtures (lane: compass-ui)

Lift `parentAgentId` and `membershipMode` from the wire `Channel` in the
live adapter (the same lift pattern the agent arm uses — "agent account
lifts parentAgentId from the agent arm", `apps/ui/src/live/adapt.test.ts:151`),
and extend the stub fixtures (`apps/ui/src/comms-stub.ts:289-290` groups,
`:314-378` channels) so `vite dev` exercises agent-attached channels in
both modes without a daemon.

Interfaces:

- Consumes: regenerated wire types from T1.
- Produces: adapter mappings `compassv1.Channel.parent_agent_id` →
  `Channel.parentAgentId` and `membership_mode` →
  `Channel.membershipMode`; fixture channels attached to fixture agents.

Test cycle: `adapt.test.ts` cases asserting both lifts, plus the H3
client-half case: a TREE `WireChannel` whose `member_account_ids`
CONTAINS the subtree agent derives `"joined"`, and the same channel with
that agent also in `subscriber_account_ids` derives `"subscribed"`. That
is the shape T4 materializes — a TREE channel's lists arrive NON-empty,
which IS the fix — so this case pins the lift, the fixture shape and the
adapter's pass-through. It deliberately does NOT assert that both-empty
lists derive non-`none`: under the chosen shape genuinely empty lists
SHOULD derive `"none"`, and `deriveMembership` (`adapt.ts:172-176`)
stays unchanged and correct in doing so. Asserting otherwise would
require `deriveMembership` to consult something beyond the two lists,
which is the per-caller-wire-field alternative leg 5 rejects. This is a
unit test over a hand-built fixture and so cannot observe what the
server produced; the regression risk lives on the server half and is
caught by T4's pgtest (derived participants in `MemberAccountIDs` with
the `channel_members` row count asserted zero). Fixture channels in
both modes exercise the bands in `vite dev`, but the eyeball is not the
acceptance; the assertions on both sides are.

## Tasks

- [ ] T1 (proto owner): `Channel.parent_agent_id = 11`,
  `Channel.membership_mode = 12`, `ChannelMembershipMode`,
  `CreateChannelRequest.parent_agent_handle = 5` /
  `.membership_mode = 6`, `rpc ReparentChannel` — additive only.
- [ ] T2 (compass-server): migration (columns, CHECKs including the
  `membership_mode IN (0, 1)` value check, indexes,
  `channel_subscriptions` **with RLS + `tenant_isolation` policy +
  grants + the account-direction index**); `CreateChannel` attach authz,
  mode, and the OWNER_ONLY-on-TREE refusal (the create half); the
  `membership_mode` immutability test;
  `Store.ReparentChannel` with every leg-3 invariant in the
  participant-gate-first order.
- [ ] T3 (compass-server): `ChannelParticipant` /
  `TopicChannelParticipant` probes beside the surviving
  `ChannelMemberExists` — derived arm on every membership gate,
  including `ListTopics`.
- [ ] T4 (compass-server): owner-set disjunct in all three predicate
  copies + `parent_agent_id`/`membership_mode` carried through those
  same three projections, `channelFromRow` and its four call sites,
  `store.Channel` and `channelToWire`, plus `CreateChannel`'s
  hand-written return literal replaced by a post-commit `getChannel`
  re-read (the fifth construction site) + participant shape in
  message/topic reads + the derived DESCENT branch in
  `loadChannelMembers`, one set-based query over the whole id set
  projecting `(channel_id, account_id)` pairs, with
  `SubscriberAccountIDs` intersected against the derived set (H3 wire
  fix) + parity tests.
- [ ] T5 (compass-server): TREE subscription overrides, the five
  delivery queries' participant-union driving relation — descent for
  the two channel-keyed sites, ascent for the three account-keyed ones,
  with `COALESCE(cs.subscribed, FALSE)` — including
  mention routing, `UpdateChannelMembers` TREE refusals, mandatory
  guards, the OWNER_ONLY-on-TREE refusal (the update half).
- [ ] T6 (compass-server): `Comms.ReparentChannel`, `CreateChannel`
  passthrough, `ChannelChanged` fan-out.
- [ ] T7 (compass-ui): single sidebar tree; `AgentTreeNode.channels`;
  home-channel single-render; the SHARED-only `groups` argument to
  `channelSections` that yields both root bands; delete the
  `New folder` button;
  `folder-*` → `tree-*` rename.
- [ ] T8 (compass-ui): live-adapter lifts + stub fixtures for both
  modes + the `adapt.test.ts` case lifting a TREE channel whose
  materialized lists contain the subtree agent.

## Open Questions

The two forks the draft carried — derived-versus-stored membership, and
the owner-set read grant — are decided by Matt and folded into Approach
legs 2 and 3 above. They are settled; do not reopen them here.

Two further items the draft filed as load-bearing questions are decisions,
not forks, and are recorded as such rather than listed below. (a) A SHARED
channel cannot hang on an agent: Approach leg 2 states it, and the T2 CHECK
`channels_group_xor_agent` makes it structural — an agent-attached channel
has `group_id IS NULL`, so no SHARED-group arm exists to fire. A question a
CHECK constraint answers is not open. (b) Groups are frozen as machinery
plus shared-spaces-only, with user-facing nesting retired: Approach leg 1
decides it and T7 implements it; `CreateChannelGroup`/`ListChannelGroups`
(`comms.proto:50,53`) stay servable, the UI just offers no affordance. The
residual forks below are what is actually open.

1. **Load-bearing — does the owner-set read grant extend to OWNER-grouped
   channels?** The ruling says agents under an owner read "all channels
   from that owner", but this record applies the new disjunct to
   agent-attached channels only, where the anchor resolves the owner; an
   OWNER-grouped channel's owner is resolvable via its group, so the
   extension is mechanical but widens read access for existing data. It is
   deferred to the ACL record, which re-cuts this surface anyway.
   Assumption designed against: agent-attached only in this record.
2. **Non-load-bearing — should the later ACL record relax the
   SHARED-on-an-agent refusal?** This record refuses it structurally (the
   CHECK). Whether the ACL record should permit a shared space to hang in
   the tree, and under what visibility, is deferred there.
   Assumption designed against: stays refused.
3. **Non-load-bearing — mode conversion.** Can an EXPLICIT channel become
   TREE, or the reverse, after create? v1 refuses, and the refusal is a
   stated invariant in Approach leg 3, not a gap here: `membership_mode`
   is immutable after create, guarded and tested (T2), because it is
   what makes the two TREE policy refusals unbypassable. What is open is
   only whether a LATER record should add conversion, which needs a
   member-rows migration story (drop rows on EXPLICIT→TREE? mint the
   derived set on TREE→EXPLICIT?) that no current need justifies.
   Assumption designed against: immutable, per the leg-3 invariant.
4. **Non-load-bearing — replacement for the dead `New folder` button.**
   Delete outright, or replace with a per-agent-row "new channel here"
   affordance calling `CreateChannel` with `parent_agent_handle`? The plan
   deletes it in T7 and defers the create affordance to a follow-up
   record. Assumption designed against: delete.
5. **Non-load-bearing — should existing user-created OWNER groups be
   auto-migrated?** The plan leaves their channels in the root band until
   manually reparented (no data migration). A one-shot server-side
   migration mapping a user's group channels under a chosen agent would
   need a group→agent mapping no data supplies. Assumption designed
   against: no auto-migration.
