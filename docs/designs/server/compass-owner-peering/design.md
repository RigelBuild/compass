# Owner peering: cross-user agent-comms authorization

Issue: RIG-2796. Sits above the handle-addressing cutover (RIG-2751, DL-271, DL-297).

## Problem

DL-271 lets a freed user handle be reclaimed by a different human, and
DL-297 says the only thing that keeps that safe is an authorization edge
between owners that does not exist yet. Today every cross-owner agent
address collapses to the merged in-band `not_found` at a per-handler
same-owner check (`Comms.OpenDM` in `go/internal/comms/comms.go`,
`lifecycleService.DespawnAsAccount` in `go/server/lifecycle.go`, the parent
check in `Comms.CreateAgent`), so two owners' fleets cannot talk at all.
This record adds the bilateral trust edge (`user_peers`, "owner peering")
that DL-271 and DL-297 both defer to, and routes cross-owner agent
addressing through it: a peer under another owner is reachable only when
both owners have approved each other, so the new owner of a reclaimed
handle gets nothing from another fleet until both sides opt in.

## Approach

### What exists today

Every cross-owner address dies at one of three same-owner checks, each returning the merged `not_found`:

- `Comms.OpenDM` (`go/internal/comms/comms.go`) resolves the peer with `resolveAgentAccount` (`go/internal/comms/resolve.go`), then compares `peer.Agent.OwnerUserID` against `store.ResolveOwner(caller)` and returns `notFoundHandle(store.ErrNotFound, peer_handle)` on a mismatch. `OpenDMAsAccount` (`go/internal/comms/agent_caller.go`) is the agent-tool path onto the same handler, and `lifecycleService.autoOpenSpawnDM` (`go/server/lifecycle.go`) is the spawn path; both are same-owner by construction.
- Member fields (`CreateChannel`, `UpdateChannelMembers`) resolve through `resolveHandles` → `Store.AccountsByHandles` → `Store.visibleAgentHandleID` (`go/internal/store/accounts.go`), whose SQL (`GetVisibleAgentHandleID` in `go/internal/store/queries/accounts.sql`) intersects the D9 account-visibility predicate: self, any user, an agent the viewer owns (`ag.owner_user_id = $1`, which matches only a user viewer), or a channel co-member. A foreign owner's agent is invisible unless already a co-member, so it misses like an unknown handle.
- Lifecycle authority is the owner's: `lifecycleService.DespawnAsAccount` (`go/server/lifecycle.go`) rejects a foreign target with `errPeerNotFound`; `Comms.CreateAgent` and `Comms.ReparentAgent` (`go/internal/comms/comms.go`) reject a foreign parent with the same merged `not_found`; `Store.ReparentAgent` clause 0 rejects a foreign caller. `SpawnAsAccount` always creates under `store.AgentOwner(caller)`.

Handle resolution is owner-namespaced over `account_handles` (`Store.AgentByHandle`, `Store.UserByHandle`, `Store.AccountsByHandles`, all in `go/internal/store/accounts.go`). Reclaim is allowed (DL-271), so `alice/x` can resolve into a stranger's fleet after `alice` is freed and re-registered.

### The edge: `user_peers`, two directed approvals

A peering is two rows in a new table `user_peers(user_id, peer_user_id, tenant_id, created_at)`, primary key `(user_id, peer_user_id)`, `CHECK (user_id <> peer_user_id)`. One row means "`user_id` approves `peer_user_id`". The edge is live only when both directions exist. There is no state column and no state machine:

- Approve: insert your directed row (idempotent, `ON CONFLICT DO NOTHING`). If the reverse row already exists, the peering is live from this commit.
- A one-sided row is a pending request. From the approver's side it is `PENDING_OUTGOING`; from the other side it is `PENDING_INCOMING`. Accepting is the same write in the reverse direction.
- Revoke: delete your directed row. The edge is gone from this commit even if the other row stays; the other user sees it as `PENDING_INCOMING` again.

The recipient of a request cannot decline or block it in this shape; it stays `PENDING_INCOMING` on their `ListPeers` until the requester revokes. Nothing is granted by a pending row and nothing is pushed to the recipient (there is no peering stream event), so the cost is one line in the recipient's own list. This is accepted for now and named in DL-392; the additive path is a `decision` column on the recipient's own row (OQ-1).

The edge keys on `user_accounts.account_id` only, never on a handle. A user rename touches one `account_handles` row and leaves every `user_peers` row alone. A reclaim registers a new account under the freed handle; that account has no rows, so nothing it owns is reachable from another fleet until both sides approve it fresh. This is the reclaim safety DL-271 defers here.

### What a peering grants: reach, never listing, never lifecycle

The D9 account-visibility predicate is inlined four times in `go/internal/store/queries/accounts.sql`: two list copies (`ListVisibleAccounts`, `AccountVisibleTo`) that answer "may the viewer enumerate this account", and two resolver copies (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) that answer "may the viewer name this handle". Today all four are textually identical (up to indentation) and the file header requires it, so the roster clip (`roster` in `go/internal/comms/roster.go`, which reads `Store.ListAccounts`) cannot drift from the member-field resolver (`Store.AccountsByHandles` → `visibleAgentHandleID`, `go/internal/store/accounts.go`).

A live peering widens only the two resolver copies, by one disjunct: an agent handle resolves for a viewer when the agent's `owner_user_id` is peered (both directions) with the viewer's owner (`COALESCE(agent_accounts.owner_user_id, viewer)`, the `Store.ResolveOwner` form). The list copies are not widened. So a peered owner's agent can be named in any handle-typed field, but it is not enumerable: it does not appear in `ListAccounts` (so no `OwnerUserId`/`HomeChannelId`/`ParentAgentId` of a foreign fleet through `accountToWire` in `go/internal/comms/mapping.go`), it cannot be a roster vantage (`resolveVisibleAgentHandle` in `go/internal/comms/resolve.go` checks `Store.AccountVisibleTo`, a list copy, so no foreign presence or activity through `PresenceFor`/`ActivityFor`), and `visibleToActor` (`go/internal/comms/subscribe.go`, also `AccountVisibleTo`) streams no `AccountChanged` for it until the two are channel co-members.

The parity rule changes from "four copies identical" to "two pairs identical, and the resolver pair equals the list pair plus exactly the peered disjunct", identical after whitespace normalization. The reason is that naming and listing are different grants: a peering is reachability, not directory sharing. The list pair is the one the roster-versus-`ListAccounts` anti-drift comment protects, and it stays identical.

T1 first lands the own-fleet widening the handle-cutover record filed as RIG-2858 (this record closes it): the `ag.owner_user_id = $1` disjunct becomes `ag.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)` in all four copies, so an agent viewer sees its own owner's other agents. Without it, an agent could name a peered `bob/y` but not its own sibling `alice/z`.

Mentions today cannot name a peered agent. `mentionRE` (`go/internal/delivery/consumer.go`) has no `/`, so `@bob/x` parses as `@bob`, and `resolveMentioned` (`go/internal/delivery/dispatch.go`) resolves every bare mention in the posting author's owner namespace with `DeliveryReads.AgentByHandle`, then requires membership (`memberSet`). This record extends the grammar to accept `@owner/agent` and resolves both forms against the channel's own agent members, each read with its owner handle and its handle. No global handle lookup runs in the consumer: it runs under the BYPASSRLS system role (`Run` in `consumer.go`), where `Store.UserByHandle` is not tenant-scoped and two tenants may both hold `bob`. The UI copy of the grammar (`MENTION_RE` in `apps/ui/src/comms.ts`) and the chip lookup (`byHandle` in `apps/ui/src/components/TopicView.tsx`, keyed on bare handle only) move in step.

Membership alone is not reach. A foreign agent can share a channel with the caller's fleet with no peering at all (owner A adds user B, who is visible to everyone under D9; B adds `b/x`, which B owns), and membership outlives a revoke because revoke deletes nothing. So the edge is enforced where a message becomes a deliver, a steer, or a wake into an agent session (§Reach), not only where a handle resolves. With the resolver widened and the gate in place, a live peering covers:

- Peer-DM open (`OpenDM`, `comms_open_dm`, `comms_dm`): the peer handle resolves through the resolver predicate, and when the peer's owner differs from the caller's, `OpenDM` also requires `Store.OwnersPeered`. Co-membership is not enough to open a DM, before or after a revoke.
- Naming a peered owner's agent in a member field (`member_handles`, `add_member_handles`, `subscribe_handles`, and the rest): resolves with no handler change. The adding owner may set `subscribed=true`, and `addOrUpdateMember` (`go/internal/store/channels.go`) pulls the peered human in as a member through `expandOwnerMembership`. Both owners approved the edge, and that approval is the consent for this; a receiving-side subscribe consent is OQ-2 option (b)3, not designed here.
- Posting inside a shared channel: the write is membership-gated (`requireChannelMember` in `go/internal/store/authz.go`); the post reaches the other owner's agents only over a live peering.
- Steering a peered agent by mention: `@owner/agent` in a shared channel, through the extended grammar. A mention of a non-member is a no-op, as today; a mention across a dead edge is dropped by the gate.
- Waking. Under DL-226 a deliver or a steer owed to an offline channel member resumes that agent's latest session, so a peered owner's post, mention, or DM can start a session on the other owner's agent and spend that owner's compute. This is a real grant and is accepted with the edge (OQ-2). The gate runs before `wake` in `fanOut` and `routeMentionsFor` (`go/internal/delivery/dispatch.go`), so an unpeered author never wakes a foreign agent.

Not granted, deliberately: spawn (a spawned peer always inherits `store.AgentOwner(caller)`), despawn, reparent, naming a peered owner's agent as a parent, roster vantage, and account listing. The lifecycle paths keep their same-owner checks unchanged. A peering is a comms edge between fleets, not shared ownership and not a shared directory.

All agents of both owners are covered. The issue's follow-up (only Managers, or a per-edge scope list) is a later record; adding an unused scope column now would be inert gating.

### Reach: one gate at delivery

Four reads decide who an authored message reaches, and the RIG-2490 recovery scan (`scanMissedMentions` in `go/internal/delivery/scan.go`) reuses the live mention path verbatim:

| Path | Read (`go/internal/store/queries/`) | Author |
| --- | --- | --- |
| live deliver, `fanOut` | `SubscribedAgents` in `delivery_reads.sql` | `$2` |
| live steer, owed row, wake: `routeMentionsFor` → `resolveMentioned` | `ChannelAgentMembers` in `delivery_reads.sql` | `$2` |
| reconnect sweep, `sweepSession` (`settle.go`) | `UndeliveredMessages` in `delivery_cursors.sql` | `m.author_account_id` |
| start-edge drain, `sweepOwedMentions` (`settle.go`) | `OwedMentions` in `delivery_cursors.sql` | `m.author_account_id` |

Each gains the same predicate on the author and the candidate recipient agent `aa` (an `agent_accounts` row):

```sql
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = <author>), <author>)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = <author>)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = <author>), <author>)
              AND p_out.peer_user_id = aa.owner_user_id)
)
```

Same owner, a system author, or a live peering. The author's owner is the `ResolveOwner` form: an agent resolves to its owner, a user to itself. A human author is gated like an agent author: the edge is between users, and a human of an unpeered owner posting into, or mention-steering, another owner's agents is the case the edge exists for. The system account is exempt: it has no owner and carries server-originated posts. The copies (the four reads and the cursor query below) are one text up to whitespace and the author expression (`$2` or `m.author_account_id`), the way the D1 sweep-set disjunct is already kept in sync across `delivery_reads.sql` and `delivery_cursors.sql`, and a parity test pins them.

The gate is a filter, not state. Approve and revoke write only the `user_peers` row. A message across a dead edge is not delivered, not steered, records no owed row, and wakes nothing. Re-approving delivers new posts and also replays any gated message still above the recipient's cursor (see below). Cost: primary-key lookups on `agent_accounts` and `user_peers` inside reads that already run, no new round trip. The delivery cursor needs one matching change: `AckDelivery` (`go/internal/store/delivery_cursors.go`) advances the contiguous cursor across seqs that are acked or self-authored (`SelfAuthoredSeqsAbove`); a gated seq is neither, so it would stop the advance and grow `above_seqs` for as long as the edge is dead. `SelfAuthoredSeqsAbove` becomes `SkippableSeqsAbove`: self-authored or out of reach at ack time. A message gated at the recipient's next ack is consumed; one still above the cursor when the edge returns replays on the next sweep. Both are fine: both owners consent at that moment.

Pull reads are not gated. `ListMessages` and `SubscribeComms` stay membership-gated, so a revoke does not hide history. Two push paths are exempt: the pinned-board sweep (`sweepPins` in `settle.go`) injects channel-curated content that any member with pin rights can unpin; `routeAskAnswerFor` (`dispatch.go`) steers an answer only to the agent that asked for it, and asking is consent for the answer.

Existing deployments: a single-owner deployment is unchanged, because every author and recipient share one owner. Where two owners already share a channel, traffic across the owner boundary stops until both run `compass peer approve`. There is no backfill; approval is the consent the edge records.

### Revoke: nothing to unwind

Revoke deletes the caller's directed row. From this commit the resolver no longer resolves the other fleet's non-co-member agents, `OpenDM` refuses every agent of that owner, and the reach gate drops every deliver, steer, and wake across the boundary in both directions, inside existing DMs and mandatory-subscription channels too. Membership, subscriptions, history, and channels are untouched, so a revoke deletes nothing and a re-approval re-adds nothing. The RevokePeer handler publishes no `ChannelChanged`: no channel changed. Membership operations between existing co-members (adding one to another channel, subscribing it) stay membership-gated as today; they grant no reach.

### Cross-owner DM name and home

A same-owner DM keeps its name `dm:<lo>:<hi>` over bare agent handles and its home in the caller's owner's reserved `__dm__` group (`dmChannelName` and `openDMTx` in `go/internal/comms/dm.go`). The name is the resume key: `UpsertDMChannelTx` (`go/internal/store/dm.go`) looks up `(group, name)` with `GetDMChannelByName` and, on a hit, `verifyReconcileDMTx` adds the wanted members to whatever channel sits there. So any reclaimable token in a cross-owner DM name is a resume-onto-a-stranger's-channel bug: name it over owner handles and a stranger who reclaims `alice`, spawns `x`, and peers with `bob` resumes onto old alice's channel and history.

A cross-owner DM is therefore named `xdm:<idLo>:<idHi>` over the two party account ids (the caller, which may be a user, and the peer agent), sorted. Ids are 32 hex characters (`newID` in `go/internal/store/ids.go`), globally unique and never reused, so the name is rename-proof and reclaim-proof and needs no handle reads. The prefix is `xdm:`, not `dm:`: a 32-hex id is a legal handle under `handleRE` (`go/internal/store/handle.go`), so a same-owner pair whose handles equal two ids would otherwise produce the same `dm:` string in the same host group and `verifyReconcileDMTx` would merge the two DMs. No same-owner name starts with `xdm:`, so the prefix keeps the two namespaces apart. The name is opaque on a name-addressed surface, which the peer-DM record already accepts for DMs ("machine-named"); the UI labels a DM by its participants, not its name (`LeftSidebar.tsx`), and the agent tools render the name only in a confirmation line.

The host owner is the lexically lower of the two owners' `account_id`s. `openDMTx` takes `LockOwnerDMTx(host)` and `EnsureOwnerDMGroupTx(host)`, so A opening on B and B opening on A compute the same host, lock, group, and name and resolve one channel. `UpsertDMChannelTx` already pulls in each party's owner through `expandOwnerMembership`, so both owners are members from birth as DL-296 states. The non-host owner reaches the channel by membership (`ChannelsByNameForViewer` in `go/internal/store/queries/channels.sql` admits a member of any channel), not by group visibility.

### Error contract: no peering is `not_found`

An unpeered foreign agent is byte-identical to an unknown handle: `CodeNotFound` naming the submitted handle, through `notFoundHandle` (`go/internal/comms/resolve.go`) and `edgeError` (`go/internal/comms/context.go`). This holds whether no row, one row, or a stale row exists between the two owners. It applies where the peering is checked: resolving a foreign agent that is not a channel co-member, and `OpenDM` on any foreign agent. A foreign co-member still resolves in a member field without a peering, as today; that grants membership, never reach (§Reach). A pending request is reported only on the peering surface (`ListPeers`), never through an addressing error. This is the DL-269 oracle invariant restated for peering, and it keeps `TestOpenDMCrossOwnerIsIndistinguishableNotFound` (`go/internal/comms/dm_open_pgtest_test.go`) and the e2e assertion in `TestCommsTenantVisibilityTransport` (`go/e2e/legcomms_tenant_test.go`) green with one new precondition: the two owners are not peered.

### Peering RPCs and the operator CLI

Three RPCs on `CommsService`, handled in `go/internal/comms` and classified `authenticatedOpen` in `classifyProcedure` (`go/internal/auth/admin_gate.go`; `classify_exhaustive_test.go` in the same package fails until every generated procedure is classified):

- `ApprovePeer(ApprovePeerRequest{peer_handle})` → `ApprovePeerResponse{Peering}`. Resolves `peer_handle` with `Store.UserByHandle`; an unknown handle, an agent handle, or the system handle is `not_found`; the caller's own handle is `invalid_argument`. Users are visible to every viewer under D9 (the `u.account_id IS NOT NULL` disjunct), so naming a user leaks nothing.
- `RevokePeer(RevokePeerRequest{peer_handle})` → `RevokePeerResponse{deleted}`. Idempotent, and `deleted` says whether a row went. This matters for the reclaim case: a user who revokes by a remembered handle that a stranger has since reclaimed resolves the stranger and deletes nothing, and a silent success would leave the real edge live. The CLI prints `no approval for <handle>` when `deleted` is false. The caller asks only about its own edges, so this is not an oracle.
- `ListPeers(ListPeersRequest{})` → `ListPeersResponse{repeated Peering}`. One entry per user with at least one row in either direction, with `state` derived from which rows exist.

`Peering{user_account_id, handle, state}` carries the id and the handle side by side (DL-270's sibling pattern). `PeeringState` is `PENDING_OUTGOING`, `PENDING_INCOMING`, `APPROVED`.

Only a user account may call the three RPCs. An agent caller (`Account.IsAgent()` on `Store.GetAccount(actor)`) gets `permission_denied`. The caller is asking about itself, so there is no existence oracle to protect. No `AgentGateway` arm is added; agents do not manage peering, so the Runner and the agent tool surface are untouched except for one line of tool copy. No stream event is added: nothing reads one today (`decodeEvent` in `apps/ui/src/live/stream.ts` drops unknown arms), and a wire arm with no reader is inert; the UI record that renders peering adds the event with its reader.

The human surface is the operator CLI: `compass peer approve <handle>`, `compass peer revoke <handle>`, `compass peer list`, on the existing `dialCommsClient` (`go/cmd/compass/client.go`), the same shape as `message post`. The bearer token comes from `resolveToken` (`$COMPASS_ADMIN_TOKEN` or `--token-file`, never argv) and must belong to the user whose edge it is; `BearerInterceptor` (`go/internal/auth/interceptor.go`) sets that account as the comms actor, so the admin approves for the admin and any user with an issued token approves for themselves.

### Tenancy

Cross-tenant peering is impossible on the request path and this record does not try to add it. `user_peers` carries `tenant_id` with the standard GUC default and gets the same `tenant_isolation` policy, FORCE RLS, and grants that `0001_init.sql` applies to every table in `tenant_tables` (DL-311). `ApprovePeer` resolves the peer through `Store.UserByHandle`, which runs under `SET LOCAL ROLE compass_app` plus the tenant GUC (`scopedDBTX` in `go/internal/store/tenant_tx.go`), so it can only ever find a same-tenant user. Isolation rests on that handle resolution under RLS, not on the foreign keys: a Postgres referential check bypasses row security, so a plain `REFERENCES user_accounts (account_id)` would accept a cross-tenant id handed to the id-typed `Store.ApprovePeer`. The composite keys `(user_id, tenant_id)` and `(peer_user_id, tenant_id)` against a new `UNIQUE (account_id, tenant_id)` on `user_accounts` make such a row fail its FK under the caller's tenant GUC, and the store maps that to `ErrInvalidArgument`; a pgtest drives it directly. The reach gate runs in the delivery consumer under the BYPASSRLS system role; it compares owner account ids, which are unique across tenants, and every `user_peers` row joins two same-tenant users by construction, so the gate cannot cross tenants either. The mention resolver reads only the channel's own members, which share the channel's tenant.

## Alternatives considered

### One symmetric row with a canonical pair and a state column

`user_peers(user_lo, user_hi, state, requested_by)` with `state IN (pending, approved)`. One row per pair is easy to list, but every write must first order the pair, and accept, revoke, and re-request are three distinct transitions on one row instead of one insert or one delete. A revoke that leaves the other side's consent recorded needs a fourth state or a second column. Two directed rows carry the same information with `INSERT ... ON CONFLICT DO NOTHING` and `DELETE` as the only writes. Rejected for the write surface, not the read surface.

### An invite token

Owner A mints an opaque token, hands it to B out of band, and B redeems it. This adds a token table, expiry, and a redeem RPC, and still needs the directed consent the two-row shape gives for free. It also cannot express "B requested first". Deferred; it can be layered on top as a way to create B's row without B knowing A's handle.

### A gate in every handler instead of the resolver predicate

Add a Go-side "owners peered" check to `resolveHandles` and every other handle-typed input one by one. Each site would carry its own copy of the rule, and a new surface could forget it. The resolver predicate is already the one place every handle-typed input resolves through, so widening it there is the smaller change. `OpenDM` is the one handler that keeps an explicit check (`Store.OwnersPeered`), because its resolver copy admits a co-member and a DM is a mandatory push into a session, which membership must never grant on its own.

### Drop the co-member disjunct for cross-owner targets in the resolver pair

Closes the co-member hole at resolution time, but changes what a member field can name today (a foreign co-member could no longer be re-added or subscribed) and still leaves delivery membership-gated, so a revoke would not stop traffic in existing channels. Rejected; the reach gate closes both holes and changes no resolver behavior for co-members.

### Gate in the Go dispatcher only

A peering read in `fanOut` and `resolveMentioned` (`go/internal/delivery/dispatch.go`) covers the live paths but not the reconnect cursor sweep or the start-edge owed drain, whose recipient sets come from SQL (`UndeliveredMessages`, `OwedMentions`). Two of four paths gated is a hole that opens on every reconnect. Rejected; the predicate goes into the four reads.

### Widen all four predicate copies (enumerate and address)

One disjunct in all four copies is the smallest edit and keeps the "four identical" header rule. But the list copies feed more than addressing: `ListAccounts` would return every foreign agent's owner, home channel, and parent (`accountToWire`), the roster would accept a foreign agent as a vantage and return the foreign fleet's presence and activity (`roster` → `PresenceFor`, `ActivityFor`), and `visibleToActor` would stream every foreign `AccountChanged` to the peer. That is directory sharing on a single approval, wider than the issue's "reachability" and wider than a Managers-only follow-up could later narrow without a breaking change. Rejected; the resolver-only widening is OQ-2 (b).

### Owner-handle DM name for the cross-owner case

`dm:<ownerLo>/<agentLo>:<ownerHi>/<agentHi>` reads well but the name is the resume key, and an owner handle is reclaimable under DL-271: the reclaimer resumes onto the previous owner's channel through `verifyReconcileDMTx`. A plain owner rename splits the conversation the same way (the next open misses and mints a second DM). Rejected; OQ-4 records the options.

### Cascade on revoke: unsubscribe or remove

Flip `subscribed = FALSE` for the revoked owner's agents, or remove them, in every shared non-DM channel. Either one mutates state the revoker does not own, protects only one direction unless both fleets are flipped, misses mandatory-subscription channels and DMs (`SubscribedAgents` in `go/internal/store/queries/delivery_reads.sql` delivers on `mandatory_subscription` regardless of the flag), and leaves a re-approval to re-add everything by hand. Rejected; OQ-5 records the options.

### A `PeeringChanged` stream event

A payload arm at tag 19 so the other side learns a request arrived. No reader exists: the UI's `decodeEvent` drops unknown arms, agents take controls from the Runner, and the CLI is one-shot. A wire arm nothing reads is inert (rule: no inert gating). Deferred to the UI record that renders peering.

### Distinct `permission_denied` for an unpeered target

Rejected. A distinct code tells the caller the handle exists under someone, which is the existence oracle DL-269 forbids for handles. A pending request would leak the same way. The peering surface (`ListPeers`) is the only place a user learns about a request, and only for their own edges.

### Peering managed through the agent gateway

An `approve_peer` arm on `CommsCallRequest` would let an agent add a trust edge for its owner. Consent belongs to the human; an agent that could widen its own reach defeats the purpose. Rejected.

## Global Constraints

- **Storage shape frozen.** `account_handles`, the two partial-unique indexes, in-place rename, and reclaim in both tiers stay exactly as DL-271 states. This record adds one table, one unique constraint on `user_accounts`, and touches no handle row.
- **Same-owner addressing stays open.** Nothing here adds a check between agents of one owner. Every same-owner pgtest that passes today must pass unchanged; the own-fleet widening only adds visibility.
- **Oracle invariant (DL-269).** Unpeered foreign, unknown, invisible, and wrong-subtype handles are one `CodeNotFound` naming the submitted handle, same code and same message. No error may reveal that a peering request is pending.
- **Lifecycle stays same-owner.** `SpawnAsAccount`, `DespawnAsAccount`, `CreateAgent` parent, `ReparentAgent`, and `Store.ReparentAgent` clause 0 are not touched.
- **Schema is a new migration.** Migrations are append-only (RIG-4077), and `0001_init.sql` is frozen. `user_peers` and the `user_accounts` unique constraint go into the next numbered `NNNN_user_peers.sql`. That file applies the `tenant_isolation` policy, FORCE RLS, and grants itself, the way 0001's `tenant_tables` loop does. It needs no backfill because the table is new. An upgrade pgtest starts from the prior version.
- **Visibility predicate parity.** In `go/internal/store/queries/accounts.sql` the two list copies (`ListVisibleAccounts`, `AccountVisibleTo`) are identical to each other, the two resolver copies (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) are identical to each other, and the resolver copy is the list copy plus exactly the peered disjunct, all compared after whitespace normalization on the text between each copy's `WHERE (` and its matching `)`. The file header states this rule; a test pins it.
- **Reach gate parity.** The reach predicate in `SubscribedAgents`, `ChannelAgentMembers`, `UndeliveredMessages`, `OwedMentions`, and `SkippableSeqsAbove` is one text, identical after whitespace normalization and with the author expression replaced by a placeholder, pinned by the SQL parity test.
- **Reach is gated at delivery, not by membership.** No message from an author of one owner is delivered, steered, recorded as owed, or used to wake an agent of another owner without a live peering, whatever the channel's membership or kind. Pull reads stay membership-gated.
- **DM names never carry a reclaimable token.** A same-owner DM keeps `dm:<lo>:<hi>` over bare agent handles (DL-294, unchanged). A cross-owner DM is named `xdm:<idLo>:<idHi>` over the two party account ids.
- **Revoke writes one row.** `RevokePeer` deletes one `user_peers` row; it never changes a member, a subscription, a channel, or a message.
- **Proto conventions.** New RPCs on `CommsService` in `comms.proto`; no new `SubscribeCommsResponse` payload arm; no reserved markers (DL-186); `moon run compass-proto:gen` regenerates all four lanes; CI gates on `compass-proto:lint`, `compass-proto:drift`, `compass-proto:gen-fence`. Every new procedure is classified in `classifyProcedure` in the same commit.
- **Trust model (D9).** No request carries a caller identity; the actor comes from `actorFromContext`. Peering RPCs act only on the caller's own edges.
- **Red → green.** Each task lands its failing test first: pgtests for store and handler tasks, `bun test` for TS.
- **Public repo.** Nothing in this record or its code names a private repo, host, or path outside this repo.

## Plan

Every task is `[compass-server]` unless tagged. Five tasks, in order, each landing with its own reader: T1 is the edge and the surface that manages it; T2 is the reach gate that reads it; T3 is addressing (resolver, `OpenDM`, DM name); T4 is mentions on both sides; T5 is the e2e proof. T1 ships nothing inert: `ListPeers` and the CLI read what `ApprovePeer` writes. T2 lands before T3 so that no addressing is ever granted while delivery is still membership-gated.

### T1 — the edge: schema, store, RPCs, door, CLI

In a new migration `go/internal/store/migrations/NNNN_user_peers.sql` (the next free version): add `UNIQUE (account_id, tenant_id)` to `user_accounts` with `ALTER TABLE` (a composite-FK target, the same technique `agent_accounts` uses with `UNIQUE (account_id, owner_user_id)`), then:

```sql
CREATE TABLE user_peers (
    user_id      TEXT NOT NULL,
    peer_user_id TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, peer_user_id),
    CHECK (user_id <> peer_user_id),
    FOREIGN KEY (user_id, tenant_id)      REFERENCES user_accounts (account_id, tenant_id) ON DELETE RESTRICT,
    FOREIGN KEY (peer_user_id, tenant_id) REFERENCES user_accounts (account_id, tenant_id) ON DELETE RESTRICT
);
CREATE INDEX user_peers_peer_idx ON user_peers (peer_user_id);
```

The same file enables and forces RLS on `user_peers`, creates the `tenant_isolation` policy with the exact expression 0001's `tenant_tables` loop uses, and issues that loop's grants to `compass_app` and `compass_system`. The RLS catalog test gains `user_peers`. No `updated_at` (rows are inserted and deleted, never updated), so it does not join `updated_at_tables`.

New query file `go/internal/store/queries/user_peers.sql` (sqlc): `InsertUserPeer :execrows` (`INSERT ... ON CONFLICT DO NOTHING`), `DeleteUserPeer :execrows`, and `ListUserPeerings :many` (a `FULL OUTER JOIN` of the caller's outgoing and incoming rows against `accounts` for the handle, yielding `peer_id, handle, outgoing bool, incoming bool`, ordered by handle). New file `go/internal/store/user_peers.go` wrapping them. `ApprovePeer` and `RevokePeer` reject `user == peer` with `ErrInvalidArgument` before any query; a composite-FK violation (an agent id, an unknown id, or a cross-tenant id) maps to `ErrInvalidArgument`.

On `proto/compass/v1/comms.proto`, add to `CommsService`:

```proto
// Approve a user as a peer of the caller (a user account). A peering is
// live only when both users have approved each other; until then this is
// a pending request. Idempotent. peer_handle is a bare user handle;
// unknown, agent, or system handles are NOT_FOUND, self is INVALID_ARGUMENT,
// an agent caller is PERMISSION_DENIED.
rpc ApprovePeer(ApprovePeerRequest) returns (ApprovePeerResponse);
// Withdraw the caller's approval of a user. Idempotent; deleted reports
// whether an approval existed. Changes no channel, member, or message.
rpc RevokePeer(RevokePeerRequest) returns (RevokePeerResponse);
// List the caller's peerings: every user with an approval in either direction.
rpc ListPeers(ListPeersRequest) returns (ListPeersResponse);
```

Messages: `ApprovePeerRequest{string peer_handle = 1}`, `ApprovePeerResponse{Peering peering = 1}`, `RevokePeerRequest{string peer_handle = 1}`, `RevokePeerResponse{bool deleted = 1}`, `ListPeersRequest{}`, `ListPeersResponse{repeated Peering peerings = 1}`, `Peering{string user_account_id = 1; string handle = 2; PeeringState state = 3}`, `enum PeeringState{PEERING_STATE_UNSPECIFIED = 0; PEERING_STATE_PENDING_OUTGOING = 1; PEERING_STATE_PENDING_INCOMING = 2; PEERING_STATE_APPROVED = 3}`. No `SubscribeCommsResponse` change. `moon run compass-proto:gen` regenerates all four lanes.

`classifyProcedure` (`go/internal/auth/admin_gate.go`): add `CommsServiceApprovePeerProcedure`, `CommsServiceRevokePeerProcedure`, `CommsServiceListPeersProcedure` to the `authenticatedOpen` CommsService arm. `admin_gate_test.go`: one row per procedure in both tables (`wantAdmin=false, wantOK=true`). `classify_exhaustive_test.go` is what turns red without this.

New file `go/internal/comms/peering.go`: `ApprovePeer`, `RevokePeer`, `ListPeers` handlers. Each resolves the actor, loads it with `Store.GetAccount`, and returns `CodePermissionDenied` unless `acc.User != nil`. `ApprovePeer`/`RevokePeer` resolve `peer_handle` with `Store.UserByHandle`; a hit whose `Account.System != nil` or whose `Account.User == nil` is `notFoundHandle(store.ErrNotFound, peer_handle)`; `peer.ID == actor` is `CodeInvalidArgument`. `ListPeers` maps `Store.ListPeerings`.

New file `go/cmd/compass/peer.go`: `newPeerCmd()` registered in `newRootCmd` (`go/cmd/compass/main.go`) beside `newMessageCmd`, with `approve <handle>`, `revoke <handle>` (`cobra.ExactArgs(1)`), and `list` (`cobra.NoArgs`), each dialing `dialCommsClient`. `approve` and `list` print one line per peering as `<handle>\t<state>`; `revoke` prints `revoked <handle>` or, when `deleted` is false, `no approval for <handle>`. The handle is a positional (it is not a credential); the token comes from `resolveToken` as every subcommand does.

Interfaces:

- `func (s *Store) ApprovePeer(ctx context.Context, user, peer AccountID) (inserted bool, err error)`
- `func (s *Store) RevokePeer(ctx context.Context, user, peer AccountID) (deleted bool, err error)`
- `func (s *Store) ListPeerings(ctx context.Context, user AccountID) ([]Peering, error)` with `type Peering struct { PeerID AccountID; Handle string; State PeeringState }` and `type PeeringState int` (`PeeringPendingOutgoing`, `PeeringPendingIncoming`, `PeeringApproved`).
- `func (c *Comms) ApprovePeer(ctx context.Context, req *connect.Request[compassv1.ApprovePeerRequest]) (*connect.Response[compassv1.ApprovePeerResponse], error)`, `RevokePeer` and `ListPeers` in the same shape; `func peeringToWire(p store.Peering) *compassv1.Peering` in `mapping.go`.
- `func newPeerCmd() *cobra.Command`; `func runPeerApprove(ctx context.Context, client compassv1connect.CommsServiceClient, handle string, out io.Writer) error`, `runPeerRevoke` (same shape), `func runPeerList(ctx context.Context, client compassv1connect.CommsServiceClient, out io.Writer) error`.
- Tests, new `user_peers_pgtest_test.go` in `go/internal/store`: approve is idempotent (`inserted=false` the second time); one row lists `PENDING_OUTGOING` on one side and `PENDING_INCOMING` on the other, both rows list `APPROVED`; revoke one side drops it to `PENDING_INCOMING` on the revoked side and returns `deleted=true`, a second revoke `deleted=false`; self-approve is `ErrInvalidArgument`; approving an agent id is `ErrInvalidArgument`; a user rename (direct `UPDATE account_handles`, the pattern `TestHandleRenameInPlaceBothTiers` in `account_handles_pgtest_test.go` uses because no store rename API exists yet) leaves the edge intact; a reclaim (peer `a` with `matt`, rename `matt` away by the same direct UPDATE, `CreateUser` a new `matt`) leaves `a`↔`oldMatt` approved and `a`↔`newMatt` absent; a direct `Store.ApprovePeer(userA, userFromTenantB)` under tenant A's context is `ErrInvalidArgument` and inserts nothing (two-tenant setup as `TestCrossTenantWriteLandsUnderWriterTenant` in `rls_pgtest_test.go`: `seedTenant` + `WithTenant`); a row written under tenant A is invisible under tenant B's context.
- Tests, new `peering_pgtest_test.go` in `go/internal/comms`: approve from an agent actor is `CodePermissionDenied`; self-approve is `CodeInvalidArgument`; approve of an agent handle or the system handle is `CodeNotFound`; `ListPeers` shows the three states; revoke by a reclaimed handle (rename the peer away, `CreateUser` a stranger under the old handle) returns `deleted=false` and leaves the real edge `APPROVED`. `admin_gate_test.go` rows above. CLI: `peer_test.go` with the `fakeComms`/`startFakeCommsServer` pattern from `message_test.go` asserting each verb's request, the bearer header, and the `no approval for <handle>` line on `deleted=false`; `TestNoTokenFlag` (`cli_test.go`) still sweeps the tree.

### T2 — the reach gate in the delivery reads

Add the §Reach predicate to `SubscribedAgents` and `ChannelAgentMembers` (`go/internal/store/queries/delivery_reads.sql`, author `$2`) and to `UndeliveredMessages` and `OwedMentions` (`go/internal/store/queries/delivery_cursors.sql`, author `m.author_account_id`; `OwedMentions` gains `JOIN agent_accounts aa ON aa.account_id = om.agent_account_id`). Rename `SelfAuthoredSeqsAbove` to `SkippableSeqsAbove`: it joins `agent_accounts aa ON aa.account_id = $3` and returns every seq above `$2` in the channel that is self-authored or fails the reach predicate for `aa`; `AckDelivery` (`go/internal/store/delivery_cursors.go`) uses it unchanged in shape, and its doc comment says "acked, self-authored, or out of reach at ack time". A gated owed row is not returned and not cleared; it drains when the edge returns, and `CountOwedMentions` counts it meanwhile. Existing pgtests post as a second, unpeered human (`author := mustUser(...)`) into another owner's agents and expect delivery, so the gate turns them red: at least `TestUndeliveredMessagesReachesUnsubscribedMandatoryMember`, `TestSetChannelPolicySeedsCursorsForNewlyMandatory`, and `TestUpdateChannelMembersConcurrentFlipSeedsLateMember` in `channel_policy_pgtest_test.go`. Those tests pin cursor seeding, not reach, so the fixture approves the edge both ways (`ApprovePeer`) before the first post, and every assertion stays as it is. Run the full `store` and `delivery` pgtest suites to find any other cross-owner fixture and fix it the same way.

Update the prose that describes these sets: the `delivery_reads.sql` header, the `DeliveryReads` method comments on `SubscribedAgents` and `ChannelAgentMembers` (`go/internal/delivery/consumer.go`), the `resolveMentioned` and `fanOut` doc comments (`go/internal/delivery/dispatch.go`, "a member out of reach is not in the set"), and the `UndeliveredMessages` doc comment on `Store`. No Go dispatcher code changes: the gate lives in the reads, so `fakeReads` (`helpers_test.go`) and every `mention_test.go` case are unchanged.

Interfaces:

- `DeliveryReads` unchanged. `db.Queries.SkippableSeqsAbove(ctx, SkippableSeqsAboveParams{ChannelID, Seq, AgentAccountID})` replaces `SelfAuthoredSeqsAbove`; `$3` is the acking agent, matched as both the self-author and the reach recipient `aa`.
- New `sql_parity_test.go` in `go/internal/store`: reads `delivery_reads.sql` and `delivery_cursors.sql` at test time, takes the text from each `-- reach:` marker to its closing `)` by paren depth, collapses every whitespace run to one space, replaces the author expression (`$2` or `m.author_account_id`) with `<author>`, and asserts the five results are equal. T3 extends it with the `accounts.sql` assertions.
- Tests, new `reach_pgtest_test.go` in `go/internal/store`, two owners A and B each with one agent, a shared channel with both agents subscribed: `SubscribedAgents` and `ChannelAgentMembers` for A's agent as author omit B's agent when unpeered or one-sided, include it when mutual, omit it after A revokes and after B revokes; the same with A the user as author; the system account as author always reaches both; a `mandatory_subscription` channel and a `kind=DM` channel behave the same; `UndeliveredMessages(B_agent)` omits A's messages posted while unpeered and returns them once the edge is live (replay by consent); `OwedMentions(B_agent)` the same for a row recorded while live; `AckDelivery` by B's agent advances the contiguous cursor across a gated seq. In `go/internal/delivery` (pgtest beside `ask_answer_recovery_pgtest_test.go`): a post by A's agent into the shared channel after revoke produces no dispatch, no wake, and no owed row for B's agent, and `@b/x` in that post is a no-op.

### T3 — addressing: own-fleet widening, peered resolver, `OpenDM`, DM name

In `go/internal/store/queries/accounts.sql`:

1. Own-fleet widening (RIG-2858; this task closes it), all four copies: the disjunct `ag.owner_user_id = $1` becomes `ag.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)`.
2. Peered disjunct, the two resolver copies (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) only:

```sql
OR EXISTS (
    SELECT 1
    FROM user_peers p_out
    JOIN user_peers p_in
      ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
    WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)
      AND p_out.peer_user_id = ag.owner_user_id
)
```

Rewrite the file-header rule: the two list copies are identical to each other, the two resolver copies are identical to each other, and a resolver copy is a list copy plus exactly the peered disjunct, all after whitespace normalization. Update the doc comments on `Store.ListAccounts` and `Store.AccountVisibleTo` (`go/internal/store/accounts.go`) that describe the disjunct list, and the comment in `resolve_pgtest_test.go` (`go/internal/comms`) that documents `AccountVisibleTo` as false for an agent-viewer/agent-target pair under one owner (no longer true).

`user_peers.sql` gains `OwnersPeered :one` (`SELECT EXISTS` over the `p_out`/`p_in` self-join in §Reach, both ids as parameters), wrapped by `Store.OwnersPeered` in `user_peers.go`.

`Comms.OpenDM` (`go/internal/comms/comms.go`): resolve the peer through the resolver predicate instead of `resolveAgentAccount`. `resolveHandles` is not the right seam because a bare handle there tries the global user index first, while a bare `OpenDM` peer is an agent in the caller's namespace today (`resolveAgentAccount`). So add `resolveAddressableAgent` to `go/internal/comms/resolve.go`: parse with `store.ParseQualifiedHandle`, pick the owner namespace with `agentOwnerNamespace` (unchanged), then call a new `Store.VisibleAgentByHandle(ctx, viewer, owner, handle)` that runs `visibleAgentHandleID` (the resolver copy, now peered-aware) and loads the hit with `GetAccount`; an empty id is `notFoundHandle(store.ErrNotFound, qh.Raw)`. `resolveVisibleAgentHandle` (the roster vantage) stays on `AccountVisibleTo`, the list copy, so a peered agent cannot be a vantage. In `OpenDM`, replace the `peer.Agent.OwnerUserID != owner` compare: `callerOwner := Store.ResolveOwner(caller)`, `peerOwner := peer.Agent.OwnerUserID`; when they differ, `Store.OwnersPeered(callerOwner, peerOwner)` must be true, else `notFoundHandle(store.ErrNotFound, peer_handle)`. The resolver admits a co-member, so this check is what keeps an unpeered or revoked co-member out of a DM. Name and host: same owner keeps `dmChannelName(callerAcc.Handle, peer.Handle)` and `host = callerOwner`; different owners use `crossOwnerDMName(caller, peer.ID)` and `host = min(callerOwner, peerOwner)`. `openDMTx(ctx, host, name, members)` is otherwise unchanged. A user caller (`callerOwner == caller`) opening on a peered agent is allowed by the same rule; the DM then holds one agent and two users, which `expandOwnerMembership` already produces.

Same-owner prose to reword, the concrete list: the `rpc OpenDM` comment ("Same-owner only — unknown and cross-owner both return NOT_FOUND") and the `OpenDMRequest.peer_handle` comment ("enforces same-owner; unknown and cross-owner both return NOT_FOUND") in `comms.proto`; the `Comms.OpenDM` doc comment ("both must share the caller's owner", "unknown OR cross-owner handle is the byte-identical merged NOT_FOUND") in `go/internal/comms/comms.go`; the `OpenDMAsAccount` doc comment ("the same-owner authz", "unknown, cross-owner, or self peer") in `go/internal/comms/agent_caller.go`; and the `openDmParameters`/`dmParameters` descriptions ("unknown or cross-owner is an error") in `packages/compass-agent/src/comms.ts`. Each becomes "same owner or a peered owner; unknown and unpeered both return NOT_FOUND". `moon run compass-proto:gen` regenerates all four lanes.

Interfaces:

- `func (c *Comms) resolveAddressableAgent(ctx context.Context, caller store.AccountID, handle string) (store.Account, error)` in `resolve.go`; `func (s *Store) VisibleAgentByHandle(ctx context.Context, viewer, owner AccountID, handle string) (Account, error)` in `go/internal/store/accounts.go`, `ErrNotFound` naming the handle on a miss, same template as `AgentByHandle`.
- `func (s *Store) OwnersPeered(ctx context.Context, a, b AccountID) (bool, error)`: true only when both directed rows exist, in either argument order. `user_peers_pgtest_test.go` (T1) gains: false with zero or one row, true with two, false again after either side revokes.
- `func crossOwnerDMName(a, b store.AccountID) string` in `go/internal/comms/dm.go`, returning `"xdm:" + lo + ":" + hi` over the sorted ids; `dmChannelName` unchanged.
- `Comms.OpenDM` signature unchanged; `OpenDMAsAccount` and `autoOpenSpawnDM` inherit the change. No signature change on `Store.ListAccounts`, `Store.AccountVisibleTo`, `Store.AccountsByHandles`.
- `openDmParameters`, `dmParameters` (arktype schemas) in `packages/compass-agent/src/comms.ts` `[compass-agent]`: description strings only; the wire is unchanged; `bun test` wire-shape asserts unchanged.
- Tests, `account_handles_pgtest_test.go` and `accounts_test.go` extend: `AccountsByHandles` from owner A resolves `b/x` only when A and B are mutually peered, and misses byte-identically to unknown when one or zero rows exist; the same from A's agent as viewer; `AccountsByHandles` from A's agent resolves a same-owner sibling with no shared channel (the own-fleet widening); `ListAccounts(A)` and `ListAccounts(A_agent)` do not include B's agents when peered (address-only); `AccountVisibleTo(A_agent, B_agent)` is false when peered and true once they share a channel; `AccountVisibleTo(A_agent, A_sibling)` is true; `roster_pgtest_test.go` shows same-owner siblings to an agent caller. `sql_parity_test.go` (T2) gains `accounts.sql`: for each of the four named queries it takes the text between the first `WHERE (` after the `-- name:` marker and the matching `)` by paren depth, collapses every whitespace run to one space, and asserts list == list, resolver == resolver, and resolver with the peered `EXISTS (...)` block removed == list.
- Tests, `dm_open_pgtest_test.go` extends: cross-owner open with a live peering creates a `kind=DM` channel named `xdm:<idLo>:<idHi>` in the lower owner's `__dm__` group with both agents and both owners as members and `created=true`; B opening on A resumes the same channel; same-owner open still names `dm:<lo>:<hi>` over handles and homes in the caller's owner's group; one-sided approval is `CodeNotFound` with a message byte-identical (after redacting the submitted handle) to the unknown-handle message, keeping `TestOpenDMCrossOwnerIsIndistinguishableNotFound` green with the "not peered" precondition; an unpeered co-member of another owner (added through a shared channel) is `CodeNotFound`; after revoke, an open on the still-co-member agent and on a third agent of the same owner are both `CodeNotFound`, while the existing DM stays listable by both parties; a same-owner sibling open from an agent caller with no shared channel succeeds; the rename/reclaim leg: peer A1↔B, open the cross-owner DM, rename A1's handle away by direct `UPDATE`, `CreateUser` a new account under the freed handle, create an agent with the same handle under it, peer it with B, open on the same agent handles is `created=true` and the old channel's member set is unchanged.

### T4 — delivery and UI: owner-qualified mentions

`mentionRE` (`go/internal/delivery/consumer.go`) becomes `(?i)@([a-z0-9][a-z0-9._-]*(?:/[a-z0-9][a-z0-9._-]*)?)`. `ChannelAgentMembers` (`delivery_reads.sql`) returns each member's owner handle and handle beside its id, joining `account_handles` twice (the agent's row `ah.account_id = aa.account_id AND ah.owner_user_id = aa.owner_user_id`, the owner's row `oh.account_id = aa.owner_user_id AND oh.owner_user_id IS NULL`). `resolveMentioned` (`go/internal/delivery/dispatch.go`) parses each non-reserved handle with `store.ParseQualifiedHandle` and matches it against that set: a bare handle matches a member whose `OwnerUserID` is `authorOwner` (`ResolveOwner`, as today) and whose `Handle` equals it; a qualified one matches on `OwnerHandle` and `Handle`. `DeliveryReads.AgentByHandle` is removed; nothing else calls it. A miss is the existing no-op. This is tenant-safe by construction: the set is the channel's own members, and the consumer's BYPASSRLS role never runs a global handle lookup. `parseMentions` dedup is on the full qualified string. An email-like `@host/path` in prose now matches one extra segment; server-side it is a no-op miss. Accepted regression: today `@alice/bob` steers the same-owner member `alice` because the match stops at `/`; after this change it is the qualified token `alice/bob`, which misses unless owner `alice` has an agent `bob` in the channel. A `mention_test.go` case pins this, and the UI chip changes the same way.

`[compass-ui]`: `MENTION_RE` (`apps/ui/src/comms.ts`) gains the same optional `/segment` group. `byHandle` in `TopicView.tsx` is keyed on both the bare handle and, for an agent, `<ownerHandle>/<handle>`, the owner handle found through `byId.get(a.ownerUserId)`; `mentionRuns` (`apps/ui/src/markdown/mention-runs.ts`) and `rehypeMentionChips` need no change because they only call `byHandle.has`.

Interfaces:

- `ChannelAgentMembers(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.ChannelAgentMember, error)` on `DeliveryReads` and `*store.Store`, with `type ChannelAgentMember struct { ID AccountID; OwnerUserID AccountID; OwnerHandle, Handle string }` in `go/internal/store/types.go`; `fakeReads.members` (`helpers_test.go`) holds that row and `fakeReads.handles` and `AgentByHandle` go away, so each `mention_test.go` case seeds its members once with their handles.
- `mentionRE` regexp; `func parseMentions(text string) []string` returns qualified strings verbatim; `resolveMentioned` signature unchanged.
- Tests (`mention_test.go` extends): `@bob/x` parses as one token `bob/x`; `@bob/` and `@/x` do not match a qualified form; a qualified mention of a member steers it; a qualified mention of a non-member is a no-op; a bare mention still resolves only in the author's owner namespace (a same-handle agent of another owner in the channel is not matched). Pgtest in `go/internal/delivery`: two tenants each holding user `bob` with agent `x`; a post in tenant A's channel mentioning `@bob/x` steers tenant A's `bob/x` and never tenant B's. UI: `comms.test.ts` parse case for the qualified form; `MarkdownText.test.tsx` renders `@bob/x` as a known chip when `byHandle` carries the qualified key and as `unknown` when it does not.

### T5 — e2e: peer, DM, revoke, restore

Extend `TestCommsTenantVisibilityTransport` (`go/e2e/legcomms_tenant_test.go`) or add a sibling test in the same file: after assertion 3 (cross-owner OpenDM is `NOT_FOUND`, now with the owners unpeered), owner 1 adds owner 2 and `owner2/agent2` to a shared channel; agent 1's `OpenDM` on `owner2/agent2` is still `NOT_FOUND` (co-member, unpeered) and a post by agent 1 does not reach agent 2; owner 1 approves owner 2 (still `NOT_FOUND`); owner 2 approves owner 1; agent 1 opens a DM on `owner2/agent2` and posts, agent 2 reads it; a post and an `@owner2/agent2` mention in the shared channel reach agent 2, and the reverse direction reaches agent 1; owner 1 revokes; `OpenDM` on `owner2/agent2` (still a co-member) and on a third agent of owner 2 are `NOT_FOUND` with the message redacted-identical to the unknown arm; a post and a qualified mention in the shared channel, in both directions, and a post into the existing DM, reach nothing; owner 1 approves again and the next shared-channel post reaches agent 2. The rename/reclaim leg stays a T3 pgtest because no public rename RPC exists to drive it.

Interfaces:

- Generated `CommsServiceClient` for `ApprovePeer`/`RevokePeer`/`ListPeers`; existing `OpenDM`/`PostMessage`/`ListMessages`/`UpdateChannelMembers` clients.
- Tests: the e2e itself (`podmanUsable` gate as today).

## Tasks

- [ ] T1 — `user_peers` + `user_accounts` composite-FK target in a new `NNNN_user_peers.sql` (policy, FORCE RLS, grants, upgrade pgtest), `queries/user_peers.sql`, `Store.ApprovePeer`/`RevokePeer`/`ListPeerings`; proto `ApprovePeer`/`RevokePeer{deleted}`/`ListPeers` + `Peering`/`PeeringState`; `classifyProcedure` + `admin_gate_test` rows; `peering.go` handlers; `compass peer approve|revoke|list`; pgtests (rename-safe, reclaim-safe, cross-tenant id rejected, reclaimed-handle revoke) + CLI tests
- [ ] T2 — reach predicate in `SubscribedAgents`, `ChannelAgentMembers`, `UndeliveredMessages`, `OwedMentions`, `SkippableSeqsAbove` (`AckDelivery`); `sql_parity_test.go` over the reach copies; doc comments; store pgtests (unpeered, one-sided, mutual, revoked, system author, DM and mandatory channels, replay on re-approve, cursor advance) + delivery pgtest (no dispatch, wake, or owed row across a dead edge)
- [ ] T3 — own-fleet widening in all four predicate copies (closes RIG-2858); peered disjunct in the two resolver copies; header rule + `accounts.sql` parity assertions; `Store.OwnersPeered`, `Store.VisibleAgentByHandle` + `resolveAddressableAgent`; `Comms.OpenDM` with `OwnersPeered` and `xdm:` naming; same-owner prose swept (five sites); agent copy; pgtests (address-only, unpeered co-member, post-revoke co-member, rename/reclaim) + `bun test`
- [x] T4 — `mentionRE` owner-qualified form; `ChannelAgentMembers` rows with owner handle + handle; `resolveMentioned` matching within the member set; `AgentByHandle` off `DeliveryReads`; two-tenant mention pgtest; UI `MENTION_RE` + `byHandle` owner/handle keys + tests
- [ ] T5 — e2e: unpeered co-member `NOT_FOUND` + no reach → one-sided `NOT_FOUND` → mutual DM, post, mention both ways → revoke: `NOT_FOUND` on co-member and third agent, nothing reaches in either direction or in the DM → re-approve restores

## Open Questions

Five forks were load-bearing. Matt ruled 2026-10-02 (RIG-4079): every recommendation stands, so the record is frozen as designed above.

### OQ-1 (load-bearing) — Edge shape, and whether a recipient can decline

Shape options:

1. **Two directed rows, mutual when both exist** (recommended; §The edge). Writes are one insert or one delete; state is derived; revoke is your own row; a reclaim has no rows. Cost: the resolver disjunct joins two rows, and `ListPeers` is a `FULL OUTER JOIN`.
2. **One symmetric row with a canonical `(lo, hi)` pair and a `state` column.** One row to read per pair; but every write orders the pair first, accept/revoke/re-request are transitions on one row, and "B revoked but A still consents" needs a second column or a fourth state.
3. **Invite token.** A minted, expiring token B redeems. Needs a token table and a redeem RPC on top of either 1 or 2, and cannot express "B asked first". Fits later as a way to create the second row.

Decline/block options (option 1 has no recipient-owned row, so a request sits as `PENDING_INCOMING` until the requester revokes; any user can plant one in any other user's list):

1. **Accept the gap for now** (recommended). A pending row grants nothing and pushes nothing (no stream event in this record), so the cost is one line on the recipient's own `ListPeers`. Named in DL-392 with the follow-up.
2. **Recipient-owned negative row**: `user_peers.decision IN (approve, block)`, or a sibling `user_peer_blocks`. Block hides the request, and makes the edge false regardless of the other row. Additive later; not needed to ship.
3. **Shape option 2 with `declined`/`blocked` as ordinary states.** Free with a state column; costs the write-ordering and the fourth-state problem above.

Recommendation: shape 1, decline 1. Under all shapes the edge keys on `user_accounts.account_id`, so rename and reclaim behave as §The edge states.

Ruled: shape 1, decline 1.

### OQ-2 (load-bearing) — What a peering grants

(a) Which agents:

1. **All agents of both owners** (recommended; §What a peering grants). No scope column.
2. **Managers only** (agents whose `agent_accounts.role` is the manager role). Smaller blast radius, but the role column is free-text set at spawn and is not a security boundary today; gating on it would make it one.
3. **Per-edge scope list** (`user_peers.scope` naming agents or roles). Most flexible, but a schema and RPC surface for a case Matt named as a follow-up, and an unused column until it exists.

(b) Address-only or enumerate-and-address:

1. **Address-only** (recommended). The peered disjunct goes into the two resolver copies of the predicate (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) and not the two list copies (`ListVisibleAccounts`, `AccountVisibleTo`). A peered agent can be named in any handle-typed field but does not appear in `ListAccounts`, cannot be a roster vantage, and emits no `AccountChanged` to the peer. Cost: the `accounts.sql` header rule "four copies identical" becomes "two pairs identical, resolver = list + the peered disjunct", pinned by a parity test. The exception exists because naming and listing are different grants; the list pair (the roster-versus-`ListAccounts` anti-drift rule) stays identical.
2. **Enumerate and address.** One disjunct in all four copies. Simpler edit, but one approval exposes the whole foreign fleet: tree topology through `accountToWire`, presence and activity through the roster, a live `AccountChanged` feed, and any of its agents force-subscribable into the approver's channels. Wider than "reachability" and hard to narrow later without a breaking change.
3. **Address-only plus receiving-side subscribe consent**: a peered agent added to a foreign channel lands unsubscribed and only its own owner may subscribe it (a rule in `UpdateChannelMembers` keyed on `expandOwnerMembership`'s owner). Safer for push-into-session, but it makes the shared-channel case a two-step handshake per channel. Fits as a follow-up if push turns out to be the problem.

(c) Waking. Under DL-226 a deliver or a steer owed to an offline agent resumes its latest session, so a peered owner's traffic starts sessions on the other owner's agents and spends that owner's compute:

1. **Accept it with the edge** (recommended; §What a peering grants). Approving a peer is consent to be woken by that peer's fleet. The reach gate keeps an unpeered or revoked author from waking anyone.
2. **Exclude cross-owner wakes.** Deliver and steer to a live session only; an offline agent gets the message on its next own-fleet-triggered start through the cursor sweep. Cheap to add (skip `wake` when the author's owner differs) but a peered mention of an offline agent then silently waits, which is the case a mention exists for.

Recommendation: (a) 1, (b) 1, (c) 1.

Ruled: (a) 1, (b) 1, (c) 1.

### OQ-3 (load-bearing) — Error a caller sees with no edge

Options:

1. **Merged `not_found`** for none, one-sided, and revoked (recommended; §Error contract). Keeps DL-269 and the existing tests. A pending request is visible only on `ListPeers`. The check sits where a peering is required: resolving a non-co-member foreign agent, and `OpenDM` on any foreign agent. A foreign co-member still resolves in a member field without a peering, as today; that is a membership grant and the reach gate keeps it from becoming delivery.
2. **Distinct `permission_denied`.** Tells the caller the target exists under someone; an existence oracle on a guessable handle.
3. **`not_found` for strangers, a distinct error once a request is pending.** Leaks the request state to an agent of the other fleet, which is exactly the fleet the human has not yet approved.

Recommendation: option 1.

Ruled: option 1.

### OQ-4 (load-bearing) — Cross-owner DM key shape

The DM name is the resume key (`GetDMChannelByName` under `UpsertDMChannelTx`, and `verifyReconcileDMTx` adds members to a hit). Options:

1. **Party account ids: `xdm:<idLo>:<idHi>`** (recommended; §Cross-owner DM name and home). Globally unique, never reused, rename-proof and reclaim-proof, no handle reads. The `xdm:` prefix keeps it out of the `dm:` namespace, where a 32-hex handle is legal and would collide. Cost: an opaque name on a name-addressed surface; the peer-DM record already calls DMs machine-named, and no surface labels a DM by its name.
2. **Owner id + agent handle: `xdm:<ownerIdLo>/<agentLo>:<ownerIdHi>/<agentHi>`.** Closes the owner-reclaim hole; still resumes wrongly after a per-owner agent-handle reclaim (the same hole same-owner `dm:<lo>:<hi>` has today under DL-294, out of scope here).
3. **Owner-handle name for display, resume keyed on a stored `(lo_id, hi_id)` pair.** Readable name and safe resume, but a new lookup column or table on `channels` and a change to the DM upsert. Largest change.

Recommendation: option 1.

Ruled: option 1.

### OQ-5 (load-bearing) — Revoke semantics for existing channels

Membership outlives a revoke, and membership alone drives every delivery read today (`SubscribedAgents`, `ChannelAgentMembers`, `UndeliveredMessages`, `OwedMentions`), so without more, a revoked fleet keeps delivering into and steering the revoker's agents, and the reverse. Co-membership can also exist with no peering at all. Options:

1. **Reach gate at delivery** (recommended; §Reach). One predicate (same owner, system author, or live peering) in the four recipient reads plus the cursor advance; `OpenDM` keeps an explicit `Store.OwnersPeered` check. Revoke writes one row and stops every deliver, steer, owed row, and wake across the boundary in both directions, in every channel kind; re-approval delivers new posts and replays any gated message still above the recipient's cursor. No state is mutated, so nothing needs re-adding. Cost: two primary-key lookups inside reads that already run per message; one renamed cursor query; two owners who already share a channel lose cross-owner delivery until both approve.
2. **Unsubscribe on revoke, non-DM channels.** `Store.RevokePeer` flips `subscribed = FALSE` for the revoked owner's agents in shared non-DM channels. Protects one direction only, mutates flags the revoker does not own, misses mandatory-subscription channels and DMs, and leaves the co-member hole open before any revoke.
3. **Remove on revoke.** Remove the foreign agents and the pulled-in owner from non-DM channels. Stops delivery there, but deletes membership, makes re-peering a re-add, and `removeMember` refuses to remove an owner while an owned agent remains, so ordering matters. Same co-member hole as 2.
4. **No cascade; report instead.** `RevokePeerResponse` lists the channels still shared so the human can sweep. Least work in the store, most for the human, and delivery continues until they act.

Recommendation: option 1.

Ruled: option 1.

### OQ-6 (non-load-bearing, deferred) — Peering UI and stream event

`ListPeers` and the `compass peer` CLI are the only surfaces in this record. A UI record adds the rendering and, with it, a `PeeringChanged` stream arm at the next free `SubscribeCommsResponse` tag, since the UI is its first reader. Nothing here blocks on it.

## Ledger delta

Rows appended to `docs/designs/DECISIONS.md` in this PR, and the existing rows each one amends:

- **DL-392 (edge shape):** Owner peering is a bilateral trust edge between two user accounts of one tenant, stored as two directed rows in `user_peers(user_id, peer_user_id)`, live only when both directions exist; approve inserts your row, revoke deletes it, state is derived and never stored. The edge keys on `account_id`, never on a handle, so rename leaves it intact and a reclaimed handle starts with no edges. A recipient cannot decline a request in this shape; a pending row grants nothing, and a recipient-owned `decision` column is the additive follow-up. Amends nothing; it is the edge DL-271 names as "the owner-peering authorization edge (RIG-2796)".
- **DL-393 (grant):** A live peering lets each owner and that owner's agents name the other owner's agents, through one new disjunct in the two resolver copies of the D9 account-visibility predicate; the two list copies are unchanged, so a peered agent is not enumerable, not a roster vantage, and emits no `AccountChanged` to the peer until it is a channel co-member. `OpenDM` resolves its peer through the same resolver and, for a foreign owner, requires `Store.OwnersPeered`. It covers peer-DM open, member fields, owner-qualified mentions in a shared channel, and the DL-226 wake those cause on the peer's offline agents. It never grants spawn, despawn, reparent, parent naming, listing, or roster vantage. The `accounts.sql` parity rule becomes: list pair identical, resolver pair identical, resolver = list + the peered disjunct, after whitespace normalization. **Amends DL-297**: the "same-owner for MVP" scope becomes "same owner, or peered owners"; DL-297's "cross-owner DMs are deferred to the bilateral owner-peering authz edge" clause is discharged by this row. DL-297 flips to `Superseded by DL-393`. Also lands the own-fleet widening the handle-cutover record filed as RIG-2858: an agent viewer sees (lists and resolves) every agent of its own owner.
- **DL-394 (error contract):** No edge, a one-sided edge, and a revoked edge are all the merged in-band `not_found` naming the submitted handle, byte-identical to an unknown handle, wherever a peering is required: resolving a foreign agent that is not a channel co-member, and `OpenDM` on any foreign agent. A foreign co-member still resolves in a member field without a peering; that is membership, not reach (DL-396). A pending request is visible only on the requester's or recipient's own `ListPeers`. **Amends DL-297's** "a cross-owner peer handle is the merged in-band `not_found`" clause (kept, with "unpeered" in place of "cross-owner") and **extends DL-269's** oracle invariant to the peering state.
- **DL-395 (cross-owner DM name and home):** A cross-owner peer-DM is named `xdm:<idLo>:<idHi>` over the two party account ids (the caller and the peer agent), never over a handle, with a prefix no same-owner `dm:` name can produce, and homed in the lower owner id's reserved `__dm__` group; the other owner reaches it by membership. Same-owner DMs keep `dm:<lo>:<hi>` over bare agent handles. A DM name is a resume key, so it never carries a reclaimable token. **Refines DL-294's** naming for the cross-owner case (DL-294 stays Active) and **refines DL-296** (members stay "both agents + pulled-in owner(s)", now always two owners on a cross-owner DM; DL-296 stays Active).
- **DL-396 (reach gate):** Delivery between owners is gated at delivery, not by membership: the four recipient reads (`SubscribedAgents`, `ChannelAgentMembers`, `UndeliveredMessages`, `OwedMentions`) and the cursor advance admit an agent only when the author shares its owner, is the system account, or is live-peered with its owner, in every channel kind. Revoke deletes one row and mutates nothing else; it stops every deliver, steer, owed row, and wake across the boundary in both directions from the next post, and re-approval restores them. Pull reads (`ListMessages`, `SubscribeComms`) stay membership-gated. **Amends DL-226**: a cross-owner wake needs a live peering. Sits beside DL-296's DM two-party floor and does not change it.
