# Compass agent peer-DM — name-addressed comms + auto-created two-way agent↔agent DMs

Status: Active
Owner: agents
Amends: compass-agent-org-mgmt-tools, compass-manager-comms-substrate,
compass-zulip-threading-model (two explicit amendments, §Approach), and extends
compass-handle-addressing-cutover (PR #698, merged, RIG-2880) to channels+topics

## Problem / Intent

Two coupled gaps, one record:

1. **Channels and topics are id-addressed and replies mis-route (T0,
   RIG-2956).** The dogfood "peer never replied" symptom was a misdiagnosis:
   on-box forensics proved the peer took its turn and replied — but the reply
   landed in the peer's own home channel, because the steer op carries no
   source channel and `comms_post_message` defaults an omitted channel to
   home. Compounding it, agents address channels by opaque `channel_id` — the
   same id-leak the merged handle-addressing cutover (PR #698 / RIG-2880)
   removed for accounts. Matt's ruling: same cutover for channels + topics —
   agents and client UIs address them only by NAME; deliveries carry the
   source channel + topic; a reply names both explicitly, never auto-picked.
2. **A Manager cannot open a conversation with the peer it spawned.** The
   dogfood small-set wave exposed it: `agents_spawn_peer` creates account +
   container + session, but the only auto-wired multi-agent channel is the
   coordination channel — structurally one-way, provisioned `owner=manager,
   OWNER_ONLY, mandatory_subscription` (`go/internal/comms/coordination.go:97-98`:
   "UPSERTs the `<handle>-coordination` channel in it (owner=manager,
   OWNER_ONLY, mandatory_subscription)") — so a report cannot reply in it.
   Matt's ruling: a peer-DM is an auto-created (resolve-if-exists) two-party
   channel containing {both agents + their human owner(s)}, with threads,
   reachable by naming the peer's handle.

The DM feature (2) is built ON the addressing cutover (1): every DM tool
returns and consumes channel names, and the reply-routing fix is what makes
"B answers A in the DM" actually land in the DM.

## Approach

Matt has ruled on every load-bearing fork this record's red-team raised
(OQ-1, 2, 4, 6, 7, 8, 9 — all now decisions below; only two non-load-bearing
deferrals remain, §Open Questions). The shape:

**One name-addressing cutover lane (T0) mirroring PR #698, then one
server-side resolve-or-create function exposed as `comms_open_dm` + a
direct-send `comms_dm` tool + spawn-time auto-open, reusing the three
mechanisms that already exist:** the coordination hook's
deterministic-name-in-reserved-group upsert as the idempotency precedent,
`ChannelKind.DM` as the topology, and `expandOwnerMembership` as the
owner-pull. No new delivery rail, no new authz model.

### Decision R1 — channels + topics addressed by NAME (the cutover lane, T0)

Matt: ids are a leak; client UIs and agents address channels and topics only
by their actual name. The precedent is the merged handle-addressing cutover
(`docs/designs/product/compass-handle-addressing-cutover/design.md`, PR #698,
RIG-2880), whose pattern this record mirrors exactly:

- **Request-INPUT** fields carry the name, resolved to an id at the service
  edge (PR #698's `go/internal/comms/resolve.go` + `AccountsByHandles`,
  owner-namespaced, visibility-scoped — an invisible name misses exactly like
  an unknown one).
- **Response / stored / event** fields STAY id-typed — DL-270 ("response,
  stored, and event account fields stay id-typed (ids are the stable join
  keys clients hold)"). This record extends DL-269/DL-270 from accounts to
  channels + topics.
- Rename-in-place proto field numbers (DL-186 posture).

Applied here:

- `comms_post_message`'s opaque optional `channel_id` param
  (`packages/compass-agent/src/comms.ts:148-152`: `"channel_id?":
  type("string")` … "Target channel; omit entirely for your home channel")
  becomes a **required channel NAME** param, resolved to a channel id at the
  comms edge. Topic is ALREADY a name (`comms.ts:137-142`: "Named
  conversation within the channel") — kept.
- **Name-resolution scope**: channel names are not globally unique —
  "Ungrouped channels (empty group) are not name-constrained"
  (`go/internal/store/channels.go:79-80`) — so the edge resolver is
  **viewer-scoped**: it resolves a name within the caller's visible channel
  set (the `channelVisiblePredicate` set, `channels.go:281-291`). A miss is
  the in-band `not_found`; an ambiguous name (two visible channels sharing
  it) is an in-band `invalid_argument` naming the ambiguity — never a silent
  pick. DM names never collide within a viewer's set (deterministic name in
  the per-owner reserved DM group, unique on the partial index
  `channels_group_name_key`, `coordination.go:148-149`).
- The same name-in/id-stays-inside treatment applies to every agent comms
  tool that takes a channel (`comms_post_message`, `comms_post_ask`,
  `comms_list_messages`); tool RESULTS render channel/topic names, wire
  responses keep ids per DL-270.
- The server-internal home-channel default (`agent_caller.go:355-367`
  `defaultChannel`) is NOT deleted: the frame-relay transcript path
  deliberately posts with "Container unset: routes to the agent's home
  channel (defaultChannel)" (`agent_caller.go:229-233`) and stays. What
  changes is the TOOL contract (R2): the agent must always name its channel.

### Decision R2 — RIG-2956 reply-routing fix (part of T0; Matt owns/drives RIG-2956)

Root cause, source-verified: the steer/deliver ops carry no source channel,
and the post tool defaults to home, so a cross-channel reply silently routes
home.

- The comms `Message` carries `topic_id` + `author_account_id` but **no
  channel** (`proto/compass/v1/comms.proto:322-336` — field 2 "was the `oneof
  container { string channel_id = 2; }` — the oneof … REMOVED … field 2 is
  reused as topic_id"; gen mirror
  `packages/compass-agent/src/gen/compass/v1/comms_pb.ts:463-500`).
- `SteerControl = {message = 1, from_handle = 2, traceparent = 3}`
  (`proto/compass/v1/agent.proto:196-213`) — no channel, and unlike
  `DeliverControl` not even a topic name. `DeliverControl = {message = 1,
  topic_name = 2, from_handle = 3, traceparent = 4}`
  (`agent.proto:227-251`) — topic name but no channel. **Both carry a
  trailing `traceparent`, so the next free tags are 4 (steer) and 5
  (deliver), not the naive 3/4.**
- `formatDeliversForPrompt` renders only
  `` `Topic ${topicId}:` `` + the generic cue "Reply via comms_post_message
  to the relevant topic." (`packages/compass-agent/src/agent.ts:734-739`) —
  no channel, and a topic *id* rather than a name.

Matt's ruling (rig2956_correction): the delivery must carry the channel and
topic it came from; a reply never auto-picks either — the agent supplies
both. The fix, folded into T0:

1. **Chosen seam: denormalize source channel NAME + topic NAME onto the
   deliver/steer controls** — `DeliverControl.channel_name = 5`;
   `SteerControl.topic_name = 4`, `SteerControl.channel_name = 5` (the
   next-free tags after each message's `traceparent`, proto-additive). This
   is the control ops' established denorm pattern, not
   a DL-270 violation: `DeliverControl.topic_name`/`from_handle` already
   carry server-resolved NAMES on the control for render ("denormalized onto
   the deliver op so the agent's turn-end coalescing queue can group held
   deliveries per topic … without a topic lookup", `agent.proto:216-222`).
   The stored `Message` is untouched (no channel field added; store/events
   stay id-typed, DL-270 intact). The server resolves once at wrap time in
   the existing `deliverOp`/`steerOp` build sites
   (`go/internal/delivery/consumer.go:374-395`), exactly where `from_handle`
   is resolved today (`authorHandle`, `consumer.go:403-415`).
2. **Render the source** in `formatDeliversForPrompt`: sections become
   `Channel <name> › topic <name>:` and the reply cue instructs supplying
   BOTH to `comms_post_message`. The agent-side plumb mirrors `fromHandle`
   (`agent.ts` `deliver(msg, fromHandle)` / `steer(msg, fromHandle)` gain the
   two names).
3. **`comms_post_message` REQUIRES channel + topic** — the home-channel
   default is dropped from the tool: `channel` (name, R1) becomes a required
   param; an omitted channel is a schema error, never a fallback. The agent
   names its home channel like any other. Same for `comms_post_ask`.
   `comms_list_messages` keeps omit-= home (a read has no misroute hazard);
   its channel param still flips to a name.

This lane's tracker issue is **RIG-2956** (re-scoped in Linear; Matt owns and
drives it). It is T0 because every DM tool consumes it.

### Decision R5 — `create_topic: true` gates topic creation EVERYWHERE

**This explicitly AMENDS the zulip-threading-model record's get-or-create
ruling** (frozen there as: "`topic` is a **required string, the topic name**
(get-or-create via `PostMessageRequest.topic_name`)" and "get-or-create makes
a typo cost one stray topic (renameable), never a lost message",
`compass-zulip-threading-model/design.md:225-232`). Matt's rationale for the
amendment (topic_autocreate ruling): "require everywhere, best against
sprawl, if we notice it being a significant blocker on DMs then we can revert
just for DMs" — the escape hatch is a later DM-only revert, not a carve-out
now.

Decision: a post to an **unknown topic name FAILS** unless `create_topic:
true` is set — in ALL channels, DMs included. It errors (naming the miss and
the flag), never mints and never loses the message. Mechanics:

- Wire: `PostMessageRequest` gains `bool create_topic = 6` (additive; fields
  1-5 verified live, `comms.proto:788-811` — container=1, blocks=2, topic
  oneof 3/4, client_request_id=5).
- Store: `AppendMessage`'s name-ref path (`go/internal/store/messages.go:29`:
  `func (s *Store) AppendMessage(ctx context.Context, m Message, channelID
  string, topic TopicRef, clientRequestID string) (Message, bool, error)`)
  today unconditionally get-or-creates ("A name-ref is get-or-created on
  (channel_id, lower(name)) via ON CONFLICT DO NOTHING + re-SELECT",
  `messages.go:183-186`). `TopicRef` gains a `Create bool`; a name miss
  without it is `ErrNotFound` (in-band at the edge), the ON CONFLICT
  get-or-create runs only when set.
- **Internal name-ref producers MUST opt into creation (amendment-safety).**
  Flipping the store default to `Create=false` breaks any server-internal
  caller that reaches the `AppendMessage` name-ref path expecting the old
  unconditional get-or-create. The load-bearing one is `postSetupThread`
  (`go/server/serve_seed.go:164-173`): it posts the root-Manager Setup thread
  to `TopicRef{Name: "Setup"}` (`serve_seed.go:49-51,173`) on first enroll,
  and the Setup topic does not pre-exist — so an un-migrated caller returns
  `ErrNotFound` and the seeded root Manager never gets its first turn (a
  production break). T0 MUST set `Create=true` on every internal producer that
  legitimately mints: today that is `postSetupThread`'s Setup seed — the one
  live production minter besides the tool edge. (`CommitAgentPost`
  (`agent_caller.go:229`) also funnels name-refs through the same
  `AppendMessage` sink (`go/internal/comms/comms.go:409-415`), but it is now a
  test-only helper — its production caller, the ConversationSink write-through,
  was removed with the sink (`agent_caller.go:11-14`) — so covering it keeps
  the test helpers green without adding a production dependency.) The gate is a
  TOOL-edge contract for agents; internal seeds are trusted minters that pass
  the flag. A T0 pgtest asserts the Setup seed still creates its topic under
  the new default.
- Tool: `comms_post_message`/`comms_post_ask`/`comms_dm` gain
  `create_topic?: boolean`; descriptions replace "an unknown name creates the
  topic" (`comms.ts:141`, `comms.ts:346-347`) with the gate.

### What already exists (verified; updated for R4)

- **The DM kind is in the data model; the GROUP_DM widening contract is
  RETIRED by this record** (see §Kinds). `proto/compass/v1/comms.proto:274-280`
  documents "a DM … widens into a GROUP_DM as members are added" with
  `CHANNEL_KIND_CHANNEL = 0`, `CHANNEL_KIND_DM = 1`, `CHANNEL_KIND_GROUP_DM
  = 2`; the store mirrors it (`go/internal/store/types.go:71-78`,
  `ChannelKindGroupDM ChannelKind = 2` at `types.go:77`). No code performs
  the widening — `UpdateChannelMembers` "only ever touches membership rows"
  (`channels.go:439-440`). Matt ruled the widening OUT (R4): the doc-comment
  contract is rewritten, not implemented.
- **Owner-pull is automatic.** `store/channels.go:194-197`:
  "expandOwnerMembership computes the final member set for a new channel: the
  requested members, plus the actor, plus the owning user of every agent in
  the set — the transitive owner-membership invariant". The DM comment above
  it is explicit (`channels.go:76-77`): "an agent↔agent DM carries both
  owners". So {A, B} in, {A, B, owner(A), owner(B)} out — zero new code.
- **DMs are ungrouped-legal and members-only-visible.** `channels.go:114-116`:
  "An ungrouped channel (DM/GROUP_DM, or a top-level channel) has no parent
  group to authorize against; the actor is a founding member by
  construction". Visibility: "the caller is a member (which governs
  DM/GROUP_DM directly …)" (`channels.go:275-276`).
- **Threads come for free.** Every channel carries the Zulip topic model
  ("A topic: a named thread within a channel — the sole message container in
  the two-level channel→topic model (D1)", `comms.proto:298-300`) and the
  post tool already requires a topic per post (`comms.ts:137-142`). A
  peer-DM is threaded the moment it exists.
- **The idempotent-upsert precedent is the coordination channel.**
  `store/coordination.go:73-77` (`EnsureOwnerCoordinationGroupTx`):
  "Deterministic + idempotent: keyed on owner_user_id + a fixed reserved
  name, so re-provisioning any manager under the same owner resolves the
  SAME group". `UpsertCoordinationChannelTx` (`coordination.go:148-156`) does
  the race-safe get-or-create: "The INSERT is `ON CONFLICT (group_id, name)
  WHERE group_id IS NOT NULL DO NOTHING` — index-inference on the PARTIAL
  unique index channels_group_name_key", under the per-owner advisory lock
  (`LockOwnerCoordinationTx`, `coordination.go:358-362`).
- **Handle→account resolution is server-side and owner-namespaced.** The
  merged cutover re-keyed `AgentByHandle` to `(owner, handle)` over
  `account_handles` and added the batch `AccountsByHandles` (PR #698 T2;
  `go/internal/store/accounts.go:631-638` in this tree: "AgentByHandle
  returns the agent account with the given handle … It returns the full
  Account so the caller owner-checks"). The DM tools take a HANDLE, never an
  account id.

### Kinds after this record: CHANNEL and DM, nothing else (R4 prose)

Matt asked what distinguishes a group DM from a regular channel; with
GROUP_DM retired the answer is: nothing worth a third kind. The final
taxonomy —

- **CHANNEL (kind=0)**: human-named, explicitly created
  (`comms_create_channel`), may live in a group (grouped visibility can be
  SHARED via the lattice, `channels.go:287-288`) or ungrouped
  (membership-only visibility, `channels.go:278`). Any party count. **A
  multi-party conversation is just a CHANNEL.**
- **DM (kind=1)**: machine-named (`dm--<handleLo>--<handleHi>`), homed in the
  per-owner reserved DM group, auto resolve-or-create (never manually
  minted), exactly **two agent parties** (+ pulled-in owners), born
  `mandatory_subscription=true`. Adding a third party is not a widening — it
  is a **conversion to a named CHANNEL** (§R4 below).
- **GROUP_DM (kind=2): RETIRED, reserve-not-delete.** The enum number is
  live in proto + both gen trees + the store
  (`comms.proto:279` `CHANNEL_KIND_GROUP_DM = 2`; `types.go:77`;
  `go/gen/compass/v1/comms.pb.go:197`; wire mapping arms
  `go/internal/comms/mapping.go:83-84` and `:256-258`), so deleting the
  number is buf-breaking (ENUM_VALUE_NO_DELETE) and violates this record's
  additive-only constraint. Retirement therefore = **the enum value STAYS in
  place, doc-comment-deprecated ("RETIRED — never produced; number reserved,
  do not reuse"), and nothing produces it**: the `mapping.go` translation
  arms for GroupDM (`mapping.go:83-84`, `:256-258`) are removed (both
  switches already default unknown → CHANNEL, `mapping.go:85-86,259-260`;
  pre-dogfood no stored kind=2 rows exist), `types.go:77`'s constant is
  deprecated in place, and the test usages migrate. A `reserved 2;` marker is
  deliberately NOT used — DL-186 stripped all reserved markers pre-dogfood,
  and keeping the deprecated value claims the number just as safely.
  **Test migration**: the existing `ChannelKindGroupDM` test channels use the
  kind only for its membership-governed visibility
  (`store/accounts_test.go:350-354` "shared-room";
  `store/channels_test.go:130-133,415-416,445-446,537-538,600-601` "collab"/
  "room"), which an **ungrouped kind=CHANNEL** provides identically — the
  visibility predicate's membership leg is kind-independent and the SHARED
  leg requires `kind = 0 AND c.group_id IS NOT NULL` (`channels.go:281-291`),
  so an ungrouped CHANNEL is members-only-visible exactly like a DM. All six
  usages flip to `Kind: ChannelKindChannel` (ungrouped, as they already are).

### The delivery trap this design must dodge (root of the dogfood symptom class)

A DM minted through bare `CreateChannel` would deliver **nothing** to the
peer: `CreateChannel` inserts every member `subscribed = FALSE`
(`channels.go:152-154`: `"INSERT INTO channel_members (channel_id,
account_id, subscribed) VALUES ($1, $2, FALSE)"`) and seeds delivery cursors
only when `c.Policy.MandatorySubscription` (`channels.go:175-178`), while the
delivery target predicate is `(cm.subscribed OR cm.channel_id =
aa.home_channel_id OR ch.mandatory_subscription)`
(`store/delivery_reads.go:35-36`, identically `delivery_cursors.go:482-483`).
An unsubscribed member of a non-mandatory non-home channel is simply not a
delivery target — the first DM message would silently reach no one. **So
peer-DMs are born `mandatory_subscription = true`** (ruled, R6 below): every
member is a delivery target regardless of the subscribed flag, and
`CreateChannel`'s existing in-tx cursor seed fires ("A channel born
mandatory_subscription=true makes every member a delivery target via the D1
disjunct … each agent member's delivery cursor MUST be seeded in this same
tx", `channels.go:163-166`). A member consequently cannot unsubscribe
(`channels.go:448-451`: "cannot unsubscribe from a mandatory-subscription
channel") — correct for a DM: you leave a DM by being removed, not by muting
it.

**Decision R6 (resolves OQ-9): `mandatory_subscription=true` stands; the
born-subscribed alternative is REJECTED.** The red-team's lighter alternative
(insert members born `subscribed=TRUE`, preserving opt-out) re-introduces the
exact D2 no-delivery hazard for agents — a DM you can silently unsubscribe
from is a self-inflicted deaf peer. Matt's ruling (dm_mandatory, option 1)
accepts the cost that the human owner accumulates unleavable DMs and routes
it UI-side: "we'll show DMs as a collapsible section or similar in the UI" —
a presentation concern, deferred with OQ-5.

### Policy: OPEN, ownerless, mandatory (ratified — closes OQ-2)

The coordination channel's `OWNER_ONLY` exists because it is a one-way
broadcast directive surface; the comms-substrate record explicitly routes
two-way traffic elsewhere: "Report→manager and lateral (report↔report)
coordination flows through direct DMs and small targeted group DMs — the
existing `ChannelKind` topology"
(`compass-manager-comms-substrate/design.md:196-198`). A peer-DM IS that
surface, so it takes the store zero-value policy: `PostPolicy = OPEN`, empty
owner — the only coherent combination (`channels.go:99-100`: "OPEN channel
must not name an owner account"). Both agents and both owners post.

### Decision R3 — reserved DM namespace with a server-enforced create-guard (resolves OQ-6)

The squat hazard the red-team found: the reserved DM group is plantable —
`requireGroupCreateAuthz` (`authz.go:87-104`) authorizes channel creation in
a group owned by the actor's owning user, so any same-owner agent (or the
human) could use the frozen org-mgmt `comms_create_channel` arm to plant
`dm--a--b` there with a hostile kind/policy/member-set, and a blind-adopt
resume would resolve every future `open_dm(a,b)` to it forever (the
deterministic name IS the key; the DM path cannot suffix around it).

Matt's ruling (dm_namespace): DMs live in a per-owner **reserved DM group**
that the manual create path is **server-forbidden** from targeting. "Should
just have a different DM namespace? a new agent could get created at any
time so we can't check in advance" — the guard, not an advance check, is the
defense. Decision:

- **PRIMARY defense — a `CreateChannel` guard (required task, T2)**: after
  the existing group-authz gate (`channels.go:117-121`), `CreateChannel`
  rejects any create whose `GroupID` resolves to a reserved DM group (the
  discriminator: the group's fixed reserved name + `VisibilityOwner`,
  symmetric with the coordination group's visibility-discriminated get-half,
  `coordination.go:87-93`). Only the OpenDM path writes there ⇒ nothing can
  squat a `dm--…` name; no in-advance existence check needed. The rejection
  is the merged in-band `not_found` (never confirms the group exists).
- **Belt-and-braces — verify-reconcile on resume (kept, demoted from primary)**:
  the OpenDM resume still verifies the resolved channel's invariants
  (`kind = DM` ∧ `mandatory_subscription = true` ∧ `{caller, peer} ⊆
  members`), reconciles recoverable drift (re-assert mandatory + missing
  member rows + seed their cursors, same tx), and returns `not_found` on a
  wrong-kind adoptee rather than adopting it. Defense-in-depth against any
  future write path into the group.

### Decision R4 — DM = exactly two parties; a third member CONVERTS it to a named CHANNEL (resolves OQ-7 + OQ-8)

Matt's ruling (groupdm_vs_channel + widening follow-up): "Adding a 3rd member
CONVERTS the DM to a named channel — the add must supply a channel name; it
becomes a normal kind=CHANNEL"; "remove the GROUP_DM option"; "the conversion
should require them to then name that channel that is no longer a dm."
Decision:

- A peer-DM is a fixed **two-party** surface (kind=DM). There is NO widening.
- A member ADD on a `kind=DM` channel is a **conversion**: in one tx the
  channel becomes `kind = CHANNEL`, takes the **caller-supplied name**
  (required — an add that omits it is rejected `invalid_argument`), and is
  re-homed OUT of the reserved DM group (ungrouped → membership-only
  visibility, unchanged for its members, `channels.go:278`), then the add
  proceeds. Conversion frees the deterministic `dm--a--b` name in the
  reserved group — so a future `open_dm(a, b)` mints a FRESH pair DM. This
  dissolves the red-team's OQ-7 permanent-unmintability finding (the widened
  channel and the pair-DM no longer contend for one name) and deletes the old
  T4 widening task with its OQ-8 `xmax = 0` insert-vs-update guard gap
  entirely — no widening, no widen-guard.
- Wire: `UpdateChannelMembersRequest` gains `string convert_channel_name = 6`
  (additive; fields 1-5 verified live, `comms.proto:655-664`). The store add
  path (`addOrUpdateMember`'s `ON CONFLICT … DO UPDATE SET subscribed`
  upsert, `channels.go:536-539`) is reached only after the conversion guard
  in `UpdateChannelMembers` runs.
- Subscribe-flips and removes on a DM need no name (they add no party);
  a remove below two parties is rejected (a one-party DM is not a thing —
  despawn/teardown, not member surgery, ends a DM).

### Trigger surface (R7 + R8): `comms_open_dm`, direct-send `comms_dm`, spawn auto-open

All three arrive on the established agent-gateway pattern
(`compass-agent-org-mgmt-tools/design.md:13`: "new arms on the existing
agent-gateway call envelopes … executed server-side under the D9
server-resolved caller").

**R7a — `comms_open_dm({ peer_handle })`, its own dedicated resolve-or-create
tool (resolves OQ-1 tool-shape).** A new `CommsService.OpenDM` RPC (public,
so the UI/human path gets the same resolve-or-create) + an `open_dm` arm on
`CommsCallRequest`/`CommsCallResult` (arms 2-6 / 2-7 live today,
`agent_gateway.proto:106-131`; the frozen org-mgmt record claims 7-9
request-side / 8-10 result-side, so this record takes **10 / 11** as a FLOOR).
Arm numbers need not be contiguous: 10/11 assumes org-mgmt lands first; if it
slips, leaving 7-9 as gaps is fine, but `open_dm` takes whatever the next-free
pair is at implementation — **never renumber down into a slot org-mgmt
reserved.** Coordinate at implementation time if either stack lands renumbered.
The server resolves `peer_handle` (owner-namespaced per PR #698's `AgentByHandle`),
enforces same-owner, and runs the upsert: ensure the per-owner reserved DM
group (the `EnsureOwnerCoordinationGroupTx` pattern, distinct reserved name),
then get-or-create the channel at the deterministic name
`dm--<handleLo>--<handleHi>` (handles sorted lexically; handles are unique
per owner post-#698, and both parties share one owner under the same-owner
gate, so the name is deterministic per pair) with `kind = DM`, members
`{caller, peer}` (owner-pull augments), zero-value policy +
`mandatory_subscription = true`, under a per-owner advisory lock with the
same `ON CONFLICT DO NOTHING` partial-index resolution. A opening on B and B
opening on A resolve identically; re-open is a resume (verify-reconciled,
R3). The tool returns the DM **channel name** + created/resumed (R1: names
out; the wire `OpenDMResponse` carries the id-typed `Channel` per DL-270, and
the TS tool renders its name).

**R7b — direct-send: `comms_dm({ peer_handle, topic, text, create_topic? })`.**
Matt's addition: "the ability to send a message/DM directly? so you just send
a DM message and the channel auto opens, and auto-creates the topic (topic is
required anyway). same with regular channel messages, but only for topics."
Chosen shape: a **dedicated `comms_dm` tool**, not a `peer_handle` param on
`comms_post_message` — the post tool stays uniformly channel-addressed (R2
just made channel+topic explicit and mandatory there; a second peer-addressed
mode inside one tool would fork its addressing contract), while `comms_dm` is
peer-addressed end to end. Agent-side it composes the two existing envelope
calls in one execute: `open_dm` (resolve-or-create, idempotent) then `post`
(carrying the broker idempotency key, `comms.ts:365-370`), so no third proto
RPC is needed and a retry after a mid-composite failure converges (open
resumes, post dedups). R5 reconciliation: the auto-open covers the CHANNEL
only — sending to a NEW topic still requires `create_topic: true` (topic
creation is gated everywhere; only manual CHANNEL creation requires the
separate `comms_create_channel`). The manual `comms_create_channel` arm stays
ONLY for minting non-DM channels: it never opens DMs and cannot target the
reserved DM group (R3's guard).

**R8 — `agents_spawn_peer` auto-opens the manager↔peer DM (resolves OQ-1
spawn-follow; now IN scope, the old record's deferral is reversed —
auto-on-spawn is CHOSEN).** `SpawnAsAccount` (`go/server/lifecycle.go:161`)
calls the same store open path after the spawn chain succeeds, and
`SpawnPeerResponse` gains `string dm_channel_name = 4` (additive — fields 1-3
verified live: `agent_account_id = 1; container_name = 2; session_id = 3`,
`agent_gateway.proto:174-178`; both response-build sites
`lifecycle.go:323-327` and `:383-387` populate it). Returned by NAME per R1.
This composes with the frozen spawn-despawn record's contract untouched
(additive field, spawn semantics unchanged); it reuses R7's server op — one
resolve-or-create, three entry points. The spawner can task its peer on the
very next tool call.

**Ownership boundary (MVP): same-owner only.** The wave shares one owner by
the F2 ruling ("A spawned agent is owned by the spawning agent's owner (the
human): all wave agents share the human `OwnerUserID`",
`compass-agent-spawn-despawn/design.md:26-27`). A cross-owner peer handle
returns the same in-band `not_found` the D9 merge uses everywhere ("An
unknown target and an other-owner target return the SAME in-band
`not_found`", `spawn-despawn/design.md:297-300`). Cross-owner DMs are
deferred (OQ-3).

**First-message delivery + turn-drive.** Once the DM exists mandatory, the
first post into it rides the frozen D1/D2 rails unchanged. The old record
gated the end-to-end loop on "RIG-2956 Defect-2 (peer turn-drive)" — that was
the MISDIAGNOSIS: forensics proved delivery and turn-drive both work (the
peer replied; the reply mis-routed). RIG-2956 is now the T0 reply-routing
lane of THIS record, so the e2e task (T6) can prove further than the old
record claimed: open → post → deliver → the peer's reply lands back in the
DM (bounded only by what a pgtest can drive without a live model turn).

### Alternatives considered

- **Auto-create on first `@mention` / inferred from message content.**
  Rejected (unchanged): inferring channel creation from message content forks
  the single-write-path invariant (DL-099: a Message is created only by an
  explicit post) and a typo'd handle would mint garbage channels. Note the
  distinction from R7b's CHOSEN direct-send: `comms_dm` addresses a peer
  handle **explicitly as a tool parameter** (validated, resolved, in-band
  error on a miss) — nothing is parsed out of message text.
- **Auto-create spawner↔peer DM at spawn time.** Previously deferred —
  **now CHOSEN (R8)**: Matt ruled auto-open at spawn with the DM returned in
  the spawn result. The old rejection ("covers only the spawner-pair")
  stands only as a reason it cannot be the ONLY mechanism; it composes with
  `comms_open_dm` for arbitrary A↔B.
- **DM→GROUP_DM widening on member add.** REMOVED (was the old T4). Matt
  ruled GROUP_DM out entirely; a third party converts the DM to a named
  CHANNEL (R4). This also deletes the widening task's `xmax = 0`
  insert-vs-update guard problem (old OQ-8) and the post-widening
  name-contention problem (old OQ-7).
- **Manual composition via the org-mgmt tools** (`comms_create_channel
  kind=DM` + `comms_update_members`). Still not the DM path, and now
  server-refused inside the reserved DM group (R3). The manual tools remain
  for arbitrary non-DM channels.
- **Member-set lookup instead of deterministic naming.** Rejected
  (unchanged): the member set is mutable, the deterministic-name key is
  immutable and reuses the proven race-safe upsert
  (`channels_group_name_key`, `coordination.go:148-149`) wholesale.
- **`peer_handle` as an optional param on `comms_post_message`** (instead of
  `comms_dm`). Rejected: it would give one tool two mutually exclusive
  addressing modes right after R2 made its channel+topic contract strict and
  explicit; a dedicated peer-addressed tool keeps both contracts simple.

## Global Constraints

- **Amendment, not rewrite — with two explicit amendments.** This record
  composes with the frozen compass-agent-org-mgmt-tools,
  compass-manager-comms-substrate, compass-agent-spawn-despawn,
  compass-handle-addressing-cutover, and compass-agent-trees records. It
  explicitly AMENDS compass-zulip-threading-model in exactly two ruled
  places: the topic get-or-create becomes `create_topic`-gated (R5) and the
  post tool's home-channel default is dropped (R2); everything else in that
  record stands.
- **Name-addressing invariant (extends DL-269/DL-270 to channels + topics).**
  Agents and client UIs address channels and topics by NAME; request-input
  channel/topic fields on the agent tool surface carry names resolved at the
  service edge; response, stored, and event fields stay id-typed. Control-op
  denorm fields (`topic_name`, `channel_name`, `from_handle`) carry
  server-resolved names for render — the established `agent.proto` pattern,
  not a DL-270 exception.
- **Additive-only proto — including the GROUP_DM retirement.** New RPC +
  messages on `comms.proto`; new oneof arms at 10/11 on the gateway
  envelopes (org-mgmt has 7-9 / 8-10 claimed,
  `compass-agent-org-mgmt-tools/design.md:81-83,100-102`); additive fields
  `PostMessageRequest.create_topic = 6`,
  `UpdateChannelMembersRequest.convert_channel_name = 6`,
  `SpawnPeerResponse.dm_channel_name = 4`, `DeliverControl.channel_name = 5`,
  `SteerControl.topic_name = 4` + `channel_name = 5` (each the next free tag
  after the message's existing `traceparent`).
  `CHANNEL_KIND_GROUP_DM = 2` is retired reserve-not-delete: the value stays
  in the enum, deprecated, never produced (deleting the number is
  buf-breaking; a `reserved` marker would re-add what DL-186 stripped). No
  renumbering, no reserved-number reuse.
- **Trust model (D9).** No request carries a caller identity; the account is
  hub-binding-resolved ("a session_id on the wire selects an account, it
  never carries one", quoted in `compass-agent-org-mgmt-tools/design.md:30`).
  New `…AsAccount` adapters guard `account == ""` → `errNoActor`.
- **Errors in-band.** Handler failures render as `CommsCallResult.error`
  (`CommsCallError{code, message}`, `agent_gateway.proto:136-139`), never a
  transport teardown. Cross-owner / unknown handle collapse to the same
  `not_found` (the D9 merge); unknown channel name and invisible channel name
  collapse identically; the reserved-group create-guard rejection is the
  same merged `not_found`.
- **Peer-DM channel invariants** (every task inherits): `kind =
  CHANNEL_KIND_DM`, exactly two agent parties, membership-only visibility,
  homed in the per-owner reserved DM group, deterministic name
  `dm--<handleLo>--<handleHi>`, `post_policy = OPEN`, empty
  `owner_account_id`, `mandatory_subscription = true`, members = both agents +
  pulled-in owner(s).
- **Runner is a pure forwarder.** No relay-proto change; the arms ride
  `RelayCommsCall` verbatim (regen only in the runner lane, per the org-mgmt
  record's T5 finding).
- **Red→green** (`rule://red-green-testing`): every task lands its failing
  test first. Gates: gofmt + golangci for Go, biome + bun test for TS.
- **Lane tags:** `[compass-server]` (proto, store, comms, hub, delivery),
  `[compass-agent]` (TS transport + tools + prompt), `[compass-runner]`
  (regen only).
- **Base note:** this record was authored against the pre-#698 working copy
  and then rebased onto post-#698 `main` (the MERGED handle cutover, PR #698 —
  `go/internal/comms/resolve.go` + `AccountsByHandles`, both present on main).
  Implementation starts from that post-#698 main (`rule://sync-before-submit`).
  File:line citations are DIRECTIONAL: the named symbols and behaviors were
  verified to exist, but #698 shifted line numbers in the files it touched
  (`agent.ts` `formatDeliversForPrompt` is at ~919 post-#698, not the 734-739
  cited inline; `consumer.go` `deliverOp`/`steerOp`/`authorHandle` at
  ~381/396/410; `mapping.go` GroupDM arms at ~85/259) — an executor re-greps
  the symbol, never trusts a bare line number. Citations for files #698 did
  not touch (`comms.proto`, `agent.proto`, store channel/coordination paths,
  `comms.ts` channel params) hold as written.

## Plan

### T0 — name-addressing + RIG-2956 reply cutover `[compass-server]` + `[compass-agent]` — tracker: RIG-2956 (Matt drives)

The general channel+topic cutover every later task consumes. Three moves:

1. **Channel-name resolve at the comms edge.** A viewer-scoped
   `ChannelByNameForViewer` resolve (mirroring PR #698's `resolve.go`
   posture): name → id within the caller-visible set
   (`channelVisiblePredicate`, `channels.go:281-291`); miss and invisible
   collapse to `not_found`; ambiguous → `invalid_argument` naming the
   collision. Wired into the agent-call adapters (`PostAsAccount`,
   `agent_caller.go:131-142`; the ask + list adapters) ahead of the store
   calls, which stay id-typed.
2. **Steer/deliver carry + render the source.** Proto:
   `DeliverControl.channel_name = 5`; `SteerControl.topic_name = 4`,
   `channel_name = 5` (`agent.proto:196-251`; next-free after `traceparent`).
   Server: resolve once at wrap
   in `deliverOp`/`steerOp` (`consumer.go:374-395`), beside `authorHandle`
   (`consumer.go:403-415`); a name-resolve miss logs and degrades exactly as
   `from_handle` does ("a handle miss never blocks a delivery",
   `consumer.go:372-373`). Agent: `deliver`/`steer` plumb the names as they
   plumb `fromHandle` today; `formatDeliversForPrompt`
   (`agent.ts:713-740`) renders `Channel <name> › topic <name>:` sections
   and a reply cue naming both required params.
3. **`comms_post_message` requires explicit channel + topic; `create_topic`
   gate.** Tool params: `channel` (name, required), `topic` (name, required,
   unchanged), `create_topic?: boolean`, `text` — the `channel_id?`/home
   default is gone (`comms.ts:148-152` replaced; descriptions at
   `comms.ts:151,346-347` rewritten). `comms_post_ask` identically;
   `comms_list_messages` keeps omit-=-home but flips to a name param. Wire:
   `PostMessageRequest.create_topic = 6`; store: `TopicRef.Create bool` gates
   the `AppendMessage` name path (`messages.go:29,183-186,206-230`) — a
   gated miss is `ErrNotFound`, in-band. The server-internal
   `defaultChannel` (`agent_caller.go:355-367`) stays for the frame-relay
   transcript path (`agent_caller.go:229-233`), which is not a tool call.

**Coordination gate (T0 ↔ RIG-2956).** T0 is both this record's foundation
(T1-T6 consume its additive proto tags + its required-channel tool contract)
AND tracker RIG-2956, which Matt drives separately. To prevent drift: RIG-2956
MUST land the exact seam this record freezes — the control-op field numbers
(`DeliverControl.channel_name = 5`, `SteerControl.topic_name = 4`/`channel_name
= 5`), the `comms_post_message` required-channel / no-home-default contract, and
the `create_topic` gate — or this record is re-frozen to match before T1
proceeds. T1+ executors block on **T0 merge**, not on RIG-2956 being "in
progress"; the corrected field numbers are mirrored into the RIG-2956 issue so
the highest-risk drift (a divergent wire tag) cannot happen silently.

**Home-channel self-posts (L2).** Because R2 drops the home default, an agent
must now NAME its own channel even for a spontaneous self-post. Its home channel
name is surfaced in the agent's system prompt / roster context (the same place
the handle is), and the viewer-scoped resolver guarantees an agent's own home
channel resolves unambiguously for it (a home channel is always visible to its
owner). Reply posts ride the delivered source channel name (T0 step 2), so only
spontaneous self-posts depend on this.

Interfaces:

- `func (s *Store) ChannelByNameForViewer(ctx context.Context, viewer AccountID, name string) (Channel, error)`
  — viewer-scoped resolve; `ErrNotFound` on miss/invisible, `ErrAmbiguous`
  (new sentinel) on a multi-hit.
- `type TopicRef struct { ID string; Name string; Create bool }` (extends the
  existing id/name union consumed at `messages.go:29`).
- Proto: `PostMessageRequest.create_topic = 6` (bool);
  `DeliverControl.channel_name = 5`; `SteerControl.topic_name = 4`,
  `SteerControl.channel_name = 5` (all string, additive — next free after
  each message's `traceparent`).
- TS: `postParameters = { text, topic, channel, create_topic? }`;
  `formatDeliversForPrompt(batch, sources)` where sources carry
  `{channelName, topicName}` per message (exact plumb mirrors
  `#deliverFromHandles`, `agent.ts:288-289`).
- Tests: pgtest — resolve hit/miss/invisible/ambiguous; gated topic post
  errors without flag, mints with flag, archived-topic revival unchanged
  when flag set; bun — post rejects omitted channel at schema; deliver/steer
  render channel+topic names; relay transcript path still lands home.

### T1 — proto: `OpenDM` RPC + gateway arm; GROUP_DM retirement; spawn + convert fields `[compass-server]`

On `comms.proto`: `rpc OpenDM(OpenDMRequest) returns (OpenDMResponse);` with

```proto
message OpenDMRequest {
  // The peer agent's handle (owner-namespaced per DL-271). The server
  // resolves it and enforces same-owner; unknown and cross-owner both
  // return not_found.
  string peer_handle = 1;
}
message OpenDMResponse {
  Channel channel = 1;  // id-typed wire per DL-270; tools render its name
  // True when this call minted the channel; false on a resume.
  bool created = 2;
}
```

Also on `comms.proto`: deprecate `CHANNEL_KIND_GROUP_DM = 2` in place
(comment: retired, never produced, number not reusable; rewrite the
`ChannelKind` doc-comment `comms.proto:274-280` — the widening promise is
replaced by the convert-to-named-channel contract);
`UpdateChannelMembersRequest.convert_channel_name = 6`;
`PostMessageRequest.create_topic = 6` (T0's field, same proto PR). On
`agent_gateway.proto`: `OpenDMRequest open_dm = 10;` in
`CommsCallRequest.call`, `OpenDMResponse open_dm = 11;` in
`CommsCallResult.result` (numbers per Global Constraints);
`SpawnPeerResponse.dm_channel_name = 4`. On `agent.proto`: T0's control
denorm fields. Refresh doc-comment arm enumerations. Regen (all four lanes,
single buf.gen).

Interfaces:

- Produces: `compassv1.OpenDMRequest{PeerHandle string}`,
  `compassv1.OpenDMResponse{Channel *Channel, Created bool}`,
  `CommsCallRequest_OpenDm` / `CommsCallResult_OpenDm` arms,
  `SpawnPeerResponse.DmChannelName`,
  `UpdateChannelMembersRequest.ConvertChannelName`, the T0 fields.
- Consumes: existing `Channel` message (unchanged).
- Test: generated-code compile; buf lint passes; grep-fence: no non-test
  producer of `CHANNEL_KIND_GROUP_DM`/`ChannelKindGroupDM` remains.

### T2 — store: reserved DM group + upsert + create-guard + convert-on-add `[compass-server]`

In `go/internal/store/` (new file `dm.go`, sibling to `coordination.go`):

- `func (s *Store) EnsureOwnerDMGroupTx(ctx context.Context, tx pgx.Tx, ownerUserID AccountID) (ChannelGroupID, error)`
  — get-or-create the per-owner reserved DM group (`VisibilityOwner`,
  un-parented, fixed reserved name), the `EnsureOwnerCoordinationGroupTx`
  pattern (`coordination.go:73-93`) including its visibility-discriminated
  get-half.
- `func (s *Store) UpsertDMChannelTx(ctx context.Context, tx pgx.Tx, spec DMChannelSpec) (ChannelID, bool, error)`
  with `type DMChannelSpec struct { GroupID ChannelGroupID; Name string; Members []AccountID }`
  — `ON CONFLICT (group_id, name) … DO NOTHING` on the partial unique index +
  re-SELECT loop (`UpsertCoordinationChannelTx` shape,
  `coordination.go:148-161`), inserting `kind = ChannelKindDM`, zero-value
  policy, `mandatory_subscription = true`; on create, expand owner
  membership, insert member rows, seed every agent member's delivery cursor
  in the same tx (the `channels.go:163-178` mandatory-create discipline).
  Resume path VERIFY-reconciles per R3 (belt): assert `kind = DM` ∧
  mandatory ∧ `{caller, peer} ⊆ members`; reconcile recoverable drift in-tx;
  wrong kind → `ErrNotFound`.
- **Create-guard (R3 primary)**: in `CreateChannel`, after the group-authz
  gate (`channels.go:117-121`), reject a create targeting a reserved DM
  group (discriminator: reserved name + `VisibilityOwner`) with the merged
  `ErrNotFound`.
- **Convert-on-add (R4)**: `UpdateChannelMembers` (`channels.go:407` area)
  gains the conversion guard ahead of the add loop: a genuine member ADD on
  a `kind = DM` channel requires `ConvertChannelName` (else
  `ErrInvalidArgument`); with it, one tx sets `kind = CHANNEL`, `name =
  ConvertChannelName`, `group_id = NULL` (leaves the reserved group — frees
  the DM name), then the existing add path (`addOrUpdateMember`,
  `channels.go:529-553`) runs. Subscribe-flips/removes need no name; a
  remove leaving <2 agent parties on a `kind=DM` channel is
  `ErrInvalidArgument`.
- Advisory lock: a sibling `LockOwnerDMTx` with a distinct key domain (so DM
  opens never serialize behind coordination reconciles).

Interfaces: as above, plus `MemberUpdatesOptions`/param carrying
`ConvertChannelName string` from the wire field.

Tests (pgtest): create → exact invariants (kind, policy, mandatory, members
incl. pulled-in owner, seeded agent cursors); second call same pair (either
direction) → same id, `created=false`; concurrent open race → one channel;
delivery-target proof via the `(cm.subscribed OR … OR
ch.mandatory_subscription)` predicate (`delivery_reads.go:35-36`) /
`SweepChannels` (`delivery_reads.go:171-179`); manual `CreateChannel` into
the reserved DM group → merged `not_found` (the squat is impossible);
squat-belt: a hand-planted wrong-kind row → resume returns `not_found`;
convert: add third agent without name → `invalid_argument`; with name →
kind=CHANNEL, renamed, ungrouped, member added, visibility unchanged for
members; `open_dm` after convert → FRESH pair DM at the freed name;
remove-below-two rejected.

### T3 — comms + hub: `OpenDM` handler, `OpenDMAsAccount`, dispatch, spawn auto-open `[compass-server]`

`Comms.OpenDM` (public handler): resolve caller from actor; peer via the
post-#698 owner-namespaced `AgentByHandle`; owner check (caller's owner ==
peer's owner; a human caller must BE the peer's owner) — failures are the
merged `not_found`; self-handle → `invalid_argument`; compute the
sorted-handle name; `store.WithTx` (`coordination.go:378-386`) around lock →
ensure group → upsert; post-commit best-effort `ChannelChanged` emit on
create (the coordination hook's emit posture, `comms/coordination.go:21-22`).
`OpenDMAsAccount` adapter on the `UpdatePinnedBoardAsAccount` pattern
(`compass-agent-org-mgmt-tools/design.md:44`); `CommsCaller` gains the
method; new `executeCall` case; update the default-case variant list +
`fakeCommsCaller`. **Spawn auto-open (R8)**: `SpawnAsAccount`
(`go/server/lifecycle.go:161`) invokes the same open path (manager ↔ new
peer) after the spawn chain succeeds and sets `DmChannelName` on both
response-build sites (`lifecycle.go:323-327` resume, `:383-387` fresh); an
open failure post-spawn is logged and returned with an empty
`dm_channel_name`, never a spawn rollback (the DM is recoverable next turn
via `comms_open_dm`; the spawned agent is not).

Interfaces:

- `func (c *Comms) OpenDM(ctx context.Context, req *connect.Request[compassv1.OpenDMRequest]) (*connect.Response[compassv1.OpenDMResponse], error)`
- `func (c *Comms) OpenDMAsAccount(ctx context.Context, account store.AccountID, req *compassv1.OpenDMRequest) (*compassv1.OpenDMResponse, error)`
- `CommsCaller` += `OpenDMAsAccount(ctx, account, req) (*compassv1.OpenDMResponse, error)`
- Tests: open on same-owner peer → channel + `created`; re-open → resume;
  cross-owner / unknown / self → in-band errors as specced; empty account →
  `errNoActor`; `ChannelChanged` reaches a member stream on create; relay
  round-trip dispatch on the new arm; spawn returns a live
  `dm_channel_name` whose channel satisfies the full invariant set;
  idempotent re-spawn returns the same DM name.

### T4 — runner: regen only `[compass-runner]`

No relay-proto change; the runner forwards `CommsCallRequest` verbatim
(pinned by `gateway_test.go:163` per the org-mgmt record's T5). Regen, no
dispatch case.

Interfaces: T1 generated arms; pure pass-through.

### T5 — agent: `comms_open_dm` + `comms_dm` tools, prompt copy `[compass-agent]`

In `packages/compass-agent/src/comms.ts` beside the existing tools:

- `comms_open_dm` (`approval: "write"`), params `{ peer_handle: string }`
  (non-blank via the `.narrow` + description idiom of `postParameters`,
  `comms.ts:120-152`), building the `open_dm` arm via the broker
  (`CommsBroker`, `comms.ts:92-117`), rendering the returned channel NAME +
  created/resumed as render-guarded text.
- `comms_dm` (`approval: "write"`), params `{ peer_handle, topic, text,
  create_topic? }`: execute awaits `open_dm`, then `post` into the resolved
  channel with the broker idempotency key (`comms.ts:365-370`); each leg's
  in-band error renders in the established `(code): message` shape.
- Prompt copy: the substrate's DM-first directive ("agents at EVERY level
  heavily prefer direct DMs / small targeted group DMs for coordination",
  `compass-manager-comms-substrate/design.md:203-205`) now names the tools:
  DM a peer with `comms_dm{peer_handle, topic, text}` (or open first with
  `comms_open_dm`); every post names its channel AND topic; a new topic
  needs `create_topic: true`.
- Consumes T0: the reworked `postParameters` and delivery render land there;
  this task only adds the two DM tools + copy.

Interfaces:

- Consumes: T1 TS gen (`OpenDMRequestSchema`, `OpenDMResponse`), T0 tool
  params.
- Produces: `comms_open_dm`, `comms_dm` registered in `createCommsTools`.
- Tests (bun, mirroring `comms.test.ts`): wire shape of open_dm; comms_dm
  composite — open-then-post ordering, idempotency key on the post leg,
  first-leg failure short-circuits, second-leg failure renders in-band;
  created vs resumed rendering; create_topic pass-through.

### T6 — e2e pgtest: open → post → deliver → reply lands in the DM `[compass-server]`

Integration proof of the full tasking loop (the old record's "stops at
delivery-target" bound is obsolete — RIG-2956's turn-drive misdiagnosis is
RESOLVED; delivery and turn-drive both work, the routing is what T0 fixed):
agent A (bound session) opens on B → channel with full invariants; A posts
(create_topic) → B is in the delivery-target set and the wrapped
deliver/steer op carries the DM's channel name + topic name; B posts a reply
naming that channel + topic → the reply lands in the SAME DM topic (not B's
home channel — the regression test for the dogfood symptom); B re-opens →
same channel; an outsider agent's `comms_list_messages` on the DM →
`not_found`; spawn-path leg: spawn returns `dm_channel_name`, manager posts
into it, peer is a delivery target. Bounded at the model boundary: the
pgtest drives B's reply as a direct post (no live LLM turn), which exercises
every rail the fix touches.

Interfaces: none new — consumes T0-T3.

## Tasks

- [ ] T0 `[compass-server]`+`[compass-agent]` — name-addressing + RIG-2956
  reply cutover: channel-name edge resolve; steer/deliver carry+render source
  channel+topic; post requires channel+topic, home default dropped,
  `create_topic` gate (tracker: RIG-2956)
- [ ] T1 `[compass-server]` — proto: `OpenDM` RPC + gateway arms 10/11;
  GROUP_DM retired in place (deprecated, never produced); `create_topic`,
  `convert_channel_name`, `dm_channel_name`, control denorm fields; regen
- [ ] T2 `[compass-server]` — store: `EnsureOwnerDMGroupTx` +
  `UpsertDMChannelTx` (+ `LockOwnerDMTx`) + reserved-group create-guard +
  verify-reconcile belt + convert-on-add + pgtests
- [ ] T3 `[compass-server]` — `Comms.OpenDM` + `OpenDMAsAccount` +
  `CommsCaller`/`executeCall` dispatch + spawn auto-open + tests
- [ ] T4 `[compass-runner]` — regen only (pure forwarder)
- [ ] T5 `[compass-agent]` — `comms_open_dm` + `comms_dm` tools + prompt copy +
  tests
- [ ] T6 `[compass-server]` — e2e pgtest: open → post → deliver → reply lands
  in the DM (incl. spawn-path leg)

## Ledger delta

Ledger-impact: applied at freeze (2026-08-30) as DL-291..297 in
`docs/designs/DECISIONS.md` §Comms & tools:

- **DL-291 (name addressing):** Agents and client UIs address channels and
  topics by NAME; request-input channel/topic fields on the agent tool
  surface are name-typed, resolved viewer-scoped at the service edge
  (unknown ≡ invisible ≡ merged `not_found`; ambiguous errors, never
  auto-picks); response/stored/event fields stay id-typed. Extends
  DL-269/DL-270 from accounts to channels + topics.
- **DL-292 (reply routing):** Steer/deliver control ops denormalize the
  source channel name + topic name (server-resolved at wrap, the
  `from_handle` pattern); the agent renders both and must name both on every
  post — `comms_post_message` has NO home-channel default and never
  auto-picks a reply target.
- **DL-293 (topic gate):** Creating a topic requires `create_topic: true`
  on the post, in every channel including DMs — amends the
  zulip-threading-model get-or-create ruling; a gated miss errors in-band,
  never mints and never drops the message. (Escape hatch per Matt: may be
  reverted for DMs only if it proves a blocker.)
- **DL-294 (DM path):** Agent↔agent DMs are auto-created
  (resolve-if-exists) by a single server-side `OpenDM` op — deterministic
  name `dm--<handleLo>--<handleHi>` in a per-owner reserved DM group —
  exposed publicly on `CommsService`, to agents as `comms_open_dm` (by peer
  handle), as the `comms_dm` direct-send composite (open + post in one
  call), and auto-opened manager↔peer at spawn
  (`SpawnPeerResponse.dm_channel_name`).
- **DL-295 (DM namespace defense):** The reserved DM group is
  server-enforced: `CreateChannel` refuses any create targeting it (merged
  `not_found`) — only the OpenDM path writes there; the resume path
  additionally verify-reconciles DM invariants (belt-and-braces).
- **DL-296 (DM shape):** A peer-DM is born `kind=DM`, exactly two agent
  parties, `post_policy=OPEN` (ownerless), `mandatory_subscription=true`,
  members = both agents + pulled-in owner(s); every member a delivery target
  from birth. A member ADD converts it — same tx — to a named `kind=CHANNEL`
  (caller MUST supply the name; the channel leaves the reserved group,
  freeing the DM name for a fresh pair-DM). `CHANNEL_KIND_GROUP_DM` is
  retired reserve-not-delete: the enum number stays, deprecated, never
  produced. The only kinds are CHANNEL and DM.
- **DL-297 (scope):** Peer-DM scope is same-owner for MVP (the F2
  wave-shares-one-owner frame); a cross-owner handle is the merged in-band
  `not_found`. Cross-owner DMs are deferred.

## Open Questions

Only two remain; both are non-load-bearing deferrals. Every other OQ from the
red-team (OQ-1, 2, 4, 6, 7, 8, 9) is now a Matt-ruled decision recorded in
§Approach/§Alternatives above.

### OQ-3 (non-load-bearing, deferred) — Cross-owner DMs

Different-owner agents cannot DM under this record (same-owner gate,
DL-297). `expandOwnerMembership` would already pull both owners in, so the
mechanism generalizes — the missing piece is an authz policy for cross-owner
contact (the bilateral owner-peering edge the handle-cutover record files as
RIG-2796-class scope). Deferred; the record is correct without it
(single-owner dogfood).

### OQ-5 (non-load-bearing, deferred) — UI DM naming + collapsible section

`dm--<handleLo>--<handleHi>` is a machine name; the UI should render DMs by
participant display names, and per Matt's R6 direction "we'll show DMs as a
collapsible section or similar in the UI" (which also absorbs the
owner-accumulates-unleavable-DMs concern from the OQ-9 ruling).
Presentation-only — no server change; the UI already has the member set.
Deferred to a UI record.
