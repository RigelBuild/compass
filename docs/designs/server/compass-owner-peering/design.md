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

The recipient of a request cannot decline or block it in this shape; it stays `PENDING_INCOMING` on their `ListPeers` until the requester revokes. Nothing is granted by a pending row and nothing is pushed to the recipient (there is no peering stream event), so the cost is one line in the recipient's own list. This is accepted for now and named in DL-372; the additive path is a `decision` column on the recipient's own row (OQ-1).

The edge keys on `user_accounts.account_id` only, never on a handle. A user rename touches one `account_handles` row and leaves every `user_peers` row alone. A reclaim registers a new account under the freed handle; that account has no rows, so nothing it owns is reachable from another fleet until both sides approve it fresh. This is the reclaim safety DL-271 defers here.

### What a peering grants: naming, never listing, never lifecycle

The D9 account-visibility predicate is inlined four times in `go/internal/store/queries/accounts.sql`: two list copies (`ListVisibleAccounts`, `AccountVisibleTo`) that answer "may the viewer enumerate this account", and two resolver copies (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) that answer "may the viewer name this handle". Today all four are textually identical and the file header requires it, so the roster clip (`roster` in `go/internal/comms/roster.go`, which reads `Store.ListAccounts`) cannot drift from the member-field resolver (`Store.AccountsByHandles` → `visibleAgentHandleID`, `go/internal/store/accounts.go`).

A live peering widens only the two resolver copies, by one disjunct: an agent handle resolves for a viewer when the agent's `owner_user_id` is peered (both directions) with the viewer's owner (`COALESCE(agent_accounts.owner_user_id, viewer)`, the `Store.ResolveOwner` form). The list copies are not widened. So a peered owner's agent can be named in any handle-typed field, but it is not enumerable: it does not appear in `ListAccounts` (so no `OwnerUserId`/`HomeChannelId`/`ParentAgentId` of a foreign fleet through `accountToWire` in `go/internal/comms/mapping.go`), it cannot be a roster vantage (`resolveVisibleAgentHandle` in `go/internal/comms/resolve.go` checks `Store.AccountVisibleTo`, a list copy, so no foreign presence or activity through `PresenceFor`/`ActivityFor`), and it emits no `AccountChanged` to the peer (`visibleToActor` in `go/internal/comms/subscribe.go` checks `AccountVisibleTo`). Once a peered agent is a channel co-member it is enumerable through the existing co-member disjunct, as any co-member is today.

The parity rule changes from "four copies identical" to "two pairs identical, and the resolver pair equals the list pair plus exactly the peered disjunct". The reason is that naming and listing are different grants: a peering is reachability, not directory sharing. The list pair is the one the roster-versus-`ListAccounts` anti-drift comment protects, and it stays identical.

The same task also lands the own-fleet widening the handle-cutover record filed as a Matt-directed follow-on: the `ag.owner_user_id = $1` disjunct becomes `ag.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)` in all four copies, so an agent viewer sees its own owner's other agents. Without it, an agent could name a peered `bob/y` but not its own sibling `alice/z`, and `OpenDM` would need a second, Go-side copy of "peered" (the drift the alternatives section rejects). With it, `OpenDM` resolves its peer through the resolver predicate and carries no owner check at all.

Mentions today cannot name a peered agent. `mentionRE` (`go/internal/delivery/consumer.go`) has no `/`, so `@bob/x` parses as `@bob`, and `resolveMentioned` (`go/internal/delivery/dispatch.go`) resolves every bare mention in the posting author's owner namespace with `DeliveryReads.AgentByHandle`. This record extends the grammar to accept `@owner/agent`, resolved as the edge does (`UserByHandle` for the owner, then `AgentByHandle`), still gated on channel membership (`memberSet`). The UI copy of the grammar (`MENTION_RE` in `apps/ui/src/comms.ts`) and the UI chip lookup (`byHandle` in `apps/ui/src/components/TopicView.tsx`, keyed on bare handle only) move in step.

With the resolver widened, the operations a peering covers are:

- Peer-DM open (`OpenDM`, `comms_open_dm`, `comms_dm`): a qualified peer handle resolves through the resolver predicate; a resolved agent under another owner is a peer.
- Naming a peered owner's agent in a member field (`member_handles`, `add_member_handles`, `subscribe_handles`, and the rest): resolves with no handler change. This is the shared-channel case peering exists for, and it has a cost worth naming: the adding owner may set `subscribed=true`, so delivers land in the peered agent's session (`SubscribedAgents` in `fanOut`, `go/internal/delivery/dispatch.go`), and `addOrUpdateMember` (`go/internal/store/channels.go`) pulls the peered human in as a member through `expandOwnerMembership`. Both owners approved the edge, and that approval is the consent for this; a receiving-side subscribe consent is OQ-2 option (b)3, not designed here.
- Posting inside a shared channel: unchanged; membership-gated (`requireChannelMember` in `go/internal/store/authz.go`).
- Steering a peered agent by mention: `@owner/agent` in a shared channel, through the extended grammar. A mention of a non-member is a no-op, as today.

Not granted, deliberately: spawn (a spawned peer always inherits `store.AgentOwner(caller)`), despawn, reparent, naming a peered owner's agent as a parent, roster vantage, and account listing. The lifecycle paths keep their same-owner checks unchanged. A peering is a comms edge between fleets, not shared ownership and not a shared directory.

All agents of both owners are covered. The issue's follow-up (only Managers, or a per-edge scope list) is a later record; adding an unused scope column now would be inert gating.

### Revoke: stop delivery, keep history

Revoke stops new addressing from this commit: the resolver disjunct needs both rows. It does not remove anyone from anything and does not rewrite history. But membership alone keeps delivery flowing: `SubscribedAgents` (`go/internal/store/queries/delivery_reads.sql`) delivers to every subscribed agent member, and `requireDMTwoParties` (`go/internal/store/channels.go`) floors only `kind = DM`, so a peered agent left subscribed in an ordinary shared channel would keep receiving the revoker's fleet's posts after the revoke. So `Store.RevokePeer` also flips `subscribed = FALSE`, in the same transaction, for every agent owned by the revoked user in every non-DM channel that the revoker or one of the revoker's agents is a member of. Membership and history stay; the peered agent can be re-subscribed by any member later. DMs are untouched: the two-party floor holds, and a DM is delivered by the mandatory flag, not the subscribed flag. Two limits, stated: a mandatory-subscription channel delivers to every member regardless of the flag (the D1 disjunct), so revoke does not stop delivery there and removal stays a member's manual step; and the revoked owner's own agents' home channels are theirs, untouched. The edge publishes `ChannelChanged` for each channel the flip touched, the same post-commit fan-out `UpdateChannelMembers` uses (`publishChannelChanged` in `go/internal/comms/mapping.go`).

### Cross-owner DM name and home

A same-owner DM keeps its name `dm:<lo>:<hi>` over bare agent handles and its home in the caller's owner's reserved `__dm__` group (`dmChannelName` and `openDMTx` in `go/internal/comms/dm.go`). The name is the resume key: `UpsertDMChannelTx` (`go/internal/store/dm.go`) looks up `(group, name)` with `GetDMChannelByName` and, on a hit, `verifyReconcileDMTx` adds the wanted members to whatever channel sits there. So any reclaimable token in a cross-owner DM name is a resume-onto-a-stranger's-channel bug: name it over owner handles and a stranger who reclaims `alice`, spawns `x`, and peers with `bob` resumes onto old alice's channel and history.

A cross-owner DM is therefore named over the two agent account ids: `dm:<agentIdLo>:<agentIdHi>`, sorted on the ids. Ids are 32 hex characters (`newID` in `go/internal/store/ids.go`), globally unique and never reused, so the name is rename-proof and reclaim-proof and needs no owner-handle reads. It is opaque on a name-addressed surface, which the peer-DM record already accepts for DMs ("machine-named"); the UI labels a DM by its participants, not its name (`LeftSidebar.tsx`), and the agent tools render the name only in a confirmation line. `:` is outside the id alphabet and the handle grammar (`handleRE` in `go/internal/store/handle.go`), so the split stays unambiguous.

The host owner is the lexically lower of the two owners' `account_id`s. `openDMTx` takes `LockOwnerDMTx(host)` and `EnsureOwnerDMGroupTx(host)`, so A opening on B and B opening on A compute the same host, lock, group, and name and resolve one channel. `UpsertDMChannelTx` already pulls in each party's owner through `expandOwnerMembership`, so both owners are members from birth as DL-296 states. The non-host owner reaches the channel by membership (`ChannelsByNameForViewer` in `go/internal/store/queries/channels.sql` admits a member of any channel), not by group visibility.

### Error contract: no peering is `not_found`

An unpeered foreign agent is byte-identical to an unknown handle: `CodeNotFound` naming the submitted handle, through `notFoundHandle` (`go/internal/comms/resolve.go`) and `edgeError` (`go/internal/comms/context.go`). This holds whether no row, one row, or a stale row exists between the two owners. A pending request is reported only on the peering surface (`ListPeers`), never through an addressing error. This is the DL-269 oracle invariant restated for peering, and it keeps `TestOpenDMCrossOwnerIsIndistinguishableNotFound` (`go/internal/comms/dm_open_pgtest_test.go`) and the e2e assertion in `TestCommsTenantVisibilityTransport` (`go/e2e/legcomms_tenant_test.go`) green with one new precondition: the two owners are not peered.

### Peering RPCs and the operator CLI

Three RPCs on `CommsService`, handled in `go/internal/comms` and classified `authenticatedOpen` in `classifyProcedure` (`go/internal/auth/admin_gate.go`; `classify_exhaustive_test.go` in the same package fails until every generated procedure is classified):

- `ApprovePeer(ApprovePeerRequest{peer_handle})` → `ApprovePeerResponse{Peering}`. Resolves `peer_handle` with `Store.UserByHandle`; an unknown handle, an agent handle, or the system handle is `not_found`; the caller's own handle is `invalid_argument`. Users are visible to every viewer under D9 (the `u.account_id IS NOT NULL` disjunct), so naming a user leaks nothing.
- `RevokePeer(RevokePeerRequest{peer_handle})` → `RevokePeerResponse{}`. Idempotent; revoking a user you never approved succeeds and flips nothing.
- `ListPeers(ListPeersRequest{})` → `ListPeersResponse{repeated Peering}`. One entry per user with at least one row in either direction, with `state` derived from which rows exist.

`Peering{user_account_id, handle, state}` carries the id and the handle side by side (DL-270's sibling pattern). `PeeringState` is `PENDING_OUTGOING`, `PENDING_INCOMING`, `APPROVED`.

Only a user account may call the three RPCs. An agent caller (`Account.IsAgent()` on `Store.GetAccount(actor)`) gets `permission_denied`. The caller is asking about itself, so there is no existence oracle to protect. No `AgentGateway` arm is added; agents do not manage peering, so the Runner and the agent tool surface are untouched except for one line of tool copy. No stream event is added: nothing reads one today (`decodeEvent` in `apps/ui/src/live/stream.ts` drops unknown arms), and a wire arm with no reader is inert; the UI record that renders peering adds the event with its reader.

The human surface is the operator CLI: `compass peer approve <handle>`, `compass peer revoke <handle>`, `compass peer list`, on the existing `dialCommsClient` (`go/cmd/compass/client.go`), the same shape as `message post`. The bearer token comes from `resolveToken` (`$COMPASS_ADMIN_TOKEN` or `--token-file`, never argv) and must belong to the user whose edge it is; `BearerInterceptor` (`go/internal/auth/interceptor.go`) sets that account as the comms actor, so the admin approves for the admin and any user with an issued token approves for themselves.

### Tenancy

Cross-tenant peering is impossible on the request path and this record does not try to add it. `user_peers` carries `tenant_id` with the standard GUC default and joins the `tenant_tables` array so the `tenant_isolation` policy applies (`0001_init.sql`, DL-311). `ApprovePeer` resolves the peer through `Store.UserByHandle`, which runs under `SET LOCAL ROLE compass_app` plus the tenant GUC (`scopedDBTX` in `go/internal/store/tenant_tx.go`), so it can only ever find a same-tenant user. Isolation rests on that handle resolution under RLS, not on the foreign keys: a Postgres referential check bypasses row security, so a plain `REFERENCES user_accounts (account_id)` would accept a cross-tenant id handed to the id-typed `Store.ApprovePeer` by a future caller. So both FKs are composite, `(user_id, tenant_id)` and `(peer_user_id, tenant_id)` against a new `UNIQUE (account_id, tenant_id)` on `user_accounts`, the technique `agent_accounts` already uses for `forge_authored_artifacts`; the row's tenant is stamped by the GUC, so a peer id from another tenant fails the FK and maps to `ErrInvalidArgument`. Under DL-317 two organizations may each hold `@matt`, so a cross-tenant edge would also need organization-qualified addressing that does not exist. A peering is an edge between two users of one tenant.

## Alternatives considered

### One symmetric row with a canonical pair and a state column

`user_peers(user_lo, user_hi, state, requested_by)` with `state IN (pending, approved)`. One row per pair is easy to list, but every write must first order the pair, and accept, revoke, and re-request are three distinct transitions on one row instead of one insert or one delete. A revoke that leaves the other side's consent recorded needs a fourth state or a second column. Two directed rows carry the same information with `INSERT ... ON CONFLICT DO NOTHING` and `DELETE` as the only writes. Rejected for the write surface, not the read surface.

### An invite token

Owner A mints an opaque token, hands it to B out of band, and B redeems it. This adds a token table, expiry, and a redeem RPC, and still needs the directed consent the two-row shape gives for free. It also cannot express "B requested first". Deferred; it can be layered on top as a way to create B's row without B knowing A's handle.

### A gate in every handler instead of the resolver predicate

Add a Go-side "owners peered" check to `OpenDM`, `resolveHandles`, and every other handle-typed input one by one. Each site would carry its own copy of the rule, and a new surface could forget it. The resolver predicate is already the one place every handle-typed input resolves through, so widening it there is the smaller and safer change. `OpenDM` today bypasses that predicate (`resolveAgentAccount` is deliberately not viewer-scoped, for the roster's dual vantage); with the own-fleet widening in place it no longer needs to, so it switches to the viewer-scoped resolver and carries no owner check of its own.

### Widen all four predicate copies (enumerate and address)

One disjunct in all four copies is the smallest edit and keeps the "four identical" header rule. But the list copies feed more than addressing: `ListAccounts` would return every foreign agent's owner, home channel, and parent (`accountToWire`), the roster would accept a foreign agent as a vantage and return the foreign fleet's presence and activity (`roster` → `PresenceFor`, `ActivityFor`), and `visibleToActor` would stream every foreign `AccountChanged` to the peer. That is directory sharing on a single approval, wider than the issue's "reachability" and wider than a Managers-only follow-up could later narrow without a breaking change. Rejected; the resolver-only widening is OQ-2 (b).

### Owner-handle DM name for the cross-owner case

`dm:<ownerLo>/<agentLo>:<ownerHi>/<agentHi>` reads well but the name is the resume key, and an owner handle is reclaimable under DL-271: the reclaimer resumes onto the previous owner's channel through `verifyReconcileDMTx`. A plain owner rename splits the conversation the same way (the next open misses and mints a second DM). Rejected; OQ-4 records the options.

### No cascade on revoke

Keep the edge as pure addressing and leave every membership and subscription alone. Simple, but a human who revokes expects the other fleet to stop reaching theirs, and the store keeps delivering to a subscribed foreign agent in an ordinary channel after the revoke (`requireDMTwoParties` floors only DMs). Nothing tells the human which channels to sweep. Rejected; OQ-5 records the options.

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
- **Schema placement follows the migration convention in force at T1.** While collapse-on-accrete holds, `user_peers` and the `user_accounts` unique constraint go into `0001_init.sql`. If the append-only boundary (RIG-4077) has been declared by then, they go into the next numbered migration instead, with no backfill needed, since the table is new. Either way `user_peers` is listed in `tenant_tables` and carries `tenant_id` with the standard GUC default.
- **Visibility predicate parity.** In `go/internal/store/queries/accounts.sql` the two list copies (`ListVisibleAccounts`, `AccountVisibleTo`) stay byte-identical to each other, the two resolver copies (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) stay byte-identical to each other, and the resolver copy is the list copy plus exactly the peered disjunct. The file header states this rule; a test pins it.
- **DM names never carry a reclaimable token.** A same-owner DM keeps `dm:<lo>:<hi>` over bare agent handles (DL-296, unchanged). A cross-owner DM is named over agent account ids only.
- **Revoke never deletes.** `RevokePeer` removes one `user_peers` row and flips `subscribed` flags; it never removes a member, a channel, or a message.
- **Proto conventions.** New RPCs on `CommsService` in `comms.proto`; no new `SubscribeCommsResponse` payload arm; no reserved markers (DL-186); `moon run compass-proto:gen` regenerates all four lanes; CI gates on `compass-proto:lint`, `compass-proto:drift`, `compass-proto:gen-fence`. Every new procedure is classified in `classifyProcedure` in the same commit.
- **Trust model (D9).** No request carries a caller identity; the actor comes from `actorFromContext`. Peering RPCs act only on the caller's own edges.
- **Red → green.** Each task lands its failing test first: pgtests for store and handler tasks, `bun test` for TS.
- **Public repo.** Nothing in this record or its code names a private repo, host, or path outside this repo.

## Plan

Every task is `[compass-server]` unless tagged. Four tasks, each landing with its own reader: T1 is the table, the store, and the predicate that reads the table; T2 is the proto, the handlers that implement it, the classification the door needs, the CLI that calls it, and the agent copy; T3 is mentions on both sides; T4 is the e2e proof.

### T1 — schema, store, and predicate: `user_peers`, own-fleet widening, peered resolver

In `go/internal/store/migrations/0001_init.sql`: add `UNIQUE (account_id, tenant_id)` to `user_accounts` (a composite-FK target, the same technique `agent_accounts` uses with `UNIQUE (account_id, owner_user_id)`), then after `account_handles`:

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

Add `'user_peers'` to the `tenant_tables` array. No `updated_at` (rows are inserted and deleted, never updated), so it does not join `updated_at_tables`.

New query file `go/internal/store/queries/user_peers.sql` (sqlc): `InsertUserPeer :execrows` (`INSERT ... ON CONFLICT DO NOTHING`), `DeleteUserPeer :execrows`, `ListUserPeerings :many` (a `FULL OUTER JOIN` of the caller's outgoing and incoming rows against `accounts` for the handle, yielding `peer_id, handle, outgoing bool, incoming bool`, ordered by handle), and `UnsubscribeOwnedAgentsFromSharedChannels :many` (`UPDATE channel_members cm SET subscribed = FALSE FROM agent_accounts aa JOIN channels ch ... WHERE aa.owner_user_id = $revoked AND cm.account_id = aa.account_id AND cm.subscribed AND ch.kind <> 1 AND EXISTS (a member row on ch for $revoker or for an agent whose owner_user_id = $revoker) RETURNING cm.channel_id`; `1` is `ChannelKindDM` in `go/internal/store/types.go`). New file `go/internal/store/user_peers.go` wrapping them. `ApprovePeer` and `RevokePeer` reject `user == peer` with `ErrInvalidArgument` before any query; a composite-FK violation (an agent id, an unknown id, or a cross-tenant id) maps to `ErrInvalidArgument`. `RevokePeer` runs the delete and the unsubscribe flip in one `beginTenantTx` transaction and returns the touched channel ids so the edge can publish `ChannelChanged`.

In `go/internal/store/queries/accounts.sql`:

1. Own-fleet widening, all four copies: the disjunct `ag.owner_user_id = $1` becomes `ag.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)`.
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

Rewrite the file-header rule: the two list copies are identical to each other, the two resolver copies are identical to each other, and a resolver copy is a list copy plus exactly the peered disjunct. Update the doc comments on `Store.ListAccounts` and `Store.AccountVisibleTo` (`go/internal/store/accounts.go`) that describe the disjunct list, and the comment in `resolve_pgtest_test.go` (`go/internal/comms`) that documents `AccountVisibleTo` as false for an agent-viewer/agent-target pair under one owner (no longer true).

Interfaces:

- `func (s *Store) ApprovePeer(ctx context.Context, user, peer AccountID) (inserted bool, err error)`
- `func (s *Store) RevokePeer(ctx context.Context, user, peer AccountID) (deleted bool, unsubscribed []ChannelID, err error)`
- `func (s *Store) ListPeerings(ctx context.Context, user AccountID) ([]Peering, error)` with `type Peering struct { PeerID AccountID; Handle string; State PeeringState }` and `type PeeringState int` (`PeeringPendingOutgoing`, `PeeringPendingIncoming`, `PeeringApproved`).
- No signature change on `Store.ListAccounts`, `Store.AccountVisibleTo`, `Store.AccountsByHandles`; they change behavior by construction.
- Tests, new `user_peers_pgtest_test.go`: approve is idempotent (`inserted=false` the second time); one row lists `PENDING_OUTGOING` on one side and `PENDING_INCOMING` on the other, both rows list `APPROVED`; revoke one side breaks the edge; self-approve is `ErrInvalidArgument`; approving an agent id is `ErrInvalidArgument`; a user rename (direct `UPDATE account_handles`, the pattern `TestHandleRenameInPlaceBothTiers` in `account_handles_pgtest_test.go` uses because no store rename API exists yet) leaves the edge intact; a reclaim (peer `a` with `matt`, rename `matt` away by the same direct UPDATE, `CreateUser` a new `matt`) leaves `a`↔`oldMatt` approved and `a`↔`newMatt` absent; a direct `Store.ApprovePeer(userA, userFromTenantB)` under tenant A's context is `ErrInvalidArgument` and inserts nothing (two-tenant setup as `TestCrossTenantWriteLandsUnderWriterTenant` in `rls_pgtest_test.go`: `seedTenant` + `WithTenant`); a row written under tenant A is invisible under tenant B's context; revoke flips `subscribed` to false for the revoked owner's agents in a shared non-DM channel and returns that channel id, leaves a DM member untouched, leaves a mandatory channel's flag untouched, and leaves membership rows in place.
- Tests, `account_handles_pgtest_test.go` and `accounts_test.go` extend: `AccountsByHandles` from owner A resolves `b/x` only when A and B are mutually peered, and misses byte-identically to unknown when one or zero rows exist; the same from A's agent as viewer; `AccountsByHandles` from A's agent resolves a same-owner sibling with no shared channel (the own-fleet widening); `ListAccounts(A)` and `ListAccounts(A_agent)` do not include B's agents when peered (address-only); `AccountVisibleTo(A_agent, B_agent)` is false when peered and true once they share a channel; `AccountVisibleTo(A_agent, A_sibling)` is true. A parity test reads `accounts.sql` at test time, extracts the four predicate bodies by their `-- name:` markers, and asserts list == list, resolver == resolver, and resolver minus the peered `EXISTS` block == list.

### T2 — proto, comms edge, door classification, CLI, agent copy

On `proto/compass/v1/comms.proto`, add to `CommsService`:

```proto
// Approve a user as a peer of the caller (a user account). A peering is
// live only when both users have approved each other; until then this is
// a pending request. Idempotent. peer_handle is a bare user handle;
// unknown, agent, or system handles are NOT_FOUND, self is INVALID_ARGUMENT,
// an agent caller is PERMISSION_DENIED.
rpc ApprovePeer(ApprovePeerRequest) returns (ApprovePeerResponse);
// Withdraw the caller's approval of a user and unsubscribe that user's agents
// from every non-DM channel the caller's fleet shares with them. Idempotent.
// Emits ChannelChanged for each channel touched.
rpc RevokePeer(RevokePeerRequest) returns (RevokePeerResponse);
// List the caller's peerings: every user with an approval in either direction.
rpc ListPeers(ListPeersRequest) returns (ListPeersResponse);
```

Messages: `ApprovePeerRequest{string peer_handle = 1}`, `ApprovePeerResponse{Peering peering = 1}`, `RevokePeerRequest{string peer_handle = 1}`, `RevokePeerResponse{}`, `ListPeersRequest{}`, `ListPeersResponse{repeated Peering peerings = 1}`, `Peering{string user_account_id = 1; string handle = 2; PeeringState state = 3}`, `enum PeeringState{PEERING_STATE_UNSPECIFIED = 0; PEERING_STATE_PENDING_OUTGOING = 1; PEERING_STATE_PENDING_INCOMING = 2; PEERING_STATE_APPROVED = 3}`. No `SubscribeCommsResponse` change. Same-owner prose to reword, the concrete list: the `rpc OpenDM` comment ("Same-owner only — unknown and cross-owner both return NOT_FOUND") and the `OpenDMRequest.peer_handle` comment ("enforces same-owner; unknown and cross-owner both return NOT_FOUND") in `comms.proto`; the `Comms.OpenDM` doc comment ("both must share the caller's owner", "unknown OR cross-owner handle is the byte-identical merged NOT_FOUND") in `go/internal/comms/comms.go`; the `OpenDMAsAccount` doc comment ("the same-owner authz", "unknown, cross-owner, or self peer") in `go/internal/comms/agent_caller.go`; and the `openDmParameters`/`dmParameters` descriptions ("unknown or cross-owner is an error") in `packages/compass-agent/src/comms.ts`. Each becomes "same owner or a peered owner; unknown and unpeered both return NOT_FOUND". `moon run compass-proto:gen` regenerates all four lanes.

`classifyProcedure` (`go/internal/auth/admin_gate.go`): add `CommsServiceApprovePeerProcedure`, `CommsServiceRevokePeerProcedure`, `CommsServiceListPeersProcedure` to the `authenticatedOpen` CommsService arm. `admin_gate_test.go`: one row per procedure in both tables (`wantAdmin=false, wantOK=true`). `classify_exhaustive_test.go` is what turns red without this.

`Comms.OpenDM` (`go/internal/comms/comms.go`): resolve the peer through the resolver predicate instead of `resolveAgentAccount`. `resolveHandles` is not the right seam because a bare handle there tries the global user index first, while a bare `OpenDM` peer is an agent in the caller's namespace today (`resolveAgentAccount`). So add `resolveAddressableAgent` to `go/internal/comms/resolve.go`: parse with `store.ParseQualifiedHandle`, pick the owner namespace with `agentOwnerNamespace` (unchanged), then call a new `Store.VisibleAgentByHandle(ctx, viewer, owner, handle)` that runs `visibleAgentHandleID` (the resolver copy, now peered-aware) and loads the hit with `GetAccount`; an empty id is `notFoundHandle(store.ErrNotFound, qh.Raw)`. `resolveVisibleAgentHandle` (the roster vantage) stays on `AccountVisibleTo`, the list copy, so a peered agent cannot be a vantage. In `OpenDM`, delete the `ResolveOwner`-and-compare block (`peer.Agent.OwnerUserID != owner`); the resolver predicate (own fleet, peered fleet, or co-member) is the whole authorization. Name and host: `callerOwner := Store.ResolveOwner(caller)`, `peerOwner := peer.Agent.OwnerUserID`; when equal, `name = dmChannelName(callerAcc.Handle, peer.Handle)` and `host = callerOwner` as today; when different, `name = dmChannelName(string(caller), string(peer.ID))` (the id pair) and `host = min(callerOwner, peerOwner)`. `openDMTx(ctx, host, name, members)` is otherwise unchanged. A user caller (`callerOwner == caller`) opening on a peered agent is allowed by the same rule; the DM then holds one agent and two users, which `expandOwnerMembership` already produces.

New file `go/internal/comms/peering.go`: `ApprovePeer`, `RevokePeer`, `ListPeers` handlers. Each resolves the actor, loads it with `Store.GetAccount`, and returns `CodePermissionDenied` unless `acc.User != nil`. `ApprovePeer`/`RevokePeer` resolve `peer_handle` with `Store.UserByHandle`; a hit whose `Account.System != nil` or whose `Account.User == nil` is `notFoundHandle(store.ErrNotFound, peer_handle)`; `peer.ID == actor` is `CodeInvalidArgument`. `RevokePeer` loads each returned channel with `Store.GetChannel` and calls `publishChannelChanged(ch, nil)` post-commit. `ListPeers` maps `Store.ListPeerings`.

New file `go/cmd/compass/peer.go`: `newPeerCmd()` registered in `newRootCmd` (`go/cmd/compass/main.go`) beside `newMessageCmd`, with `approve <handle>`, `revoke <handle>` (`cobra.ExactArgs(1)`), and `list` (`cobra.NoArgs`), each dialing `dialCommsClient` and printing one line per peering as `<handle>\t<state>`. The handle is a positional (it is not a credential); the token comes from `resolveToken` as every subcommand does.

Interfaces:

- `func (c *Comms) ApprovePeer(ctx context.Context, req *connect.Request[compassv1.ApprovePeerRequest]) (*connect.Response[compassv1.ApprovePeerResponse], error)`
- `func (c *Comms) RevokePeer(ctx context.Context, req *connect.Request[compassv1.RevokePeerRequest]) (*connect.Response[compassv1.RevokePeerResponse], error)`
- `func (c *Comms) ListPeers(ctx context.Context, req *connect.Request[compassv1.ListPeersRequest]) (*connect.Response[compassv1.ListPeersResponse], error)`
- `func peeringToWire(p store.Peering) *compassv1.Peering` in `mapping.go`.
- `func (c *Comms) resolveAddressableAgent(ctx context.Context, caller store.AccountID, handle string) (store.Account, error)` in `resolve.go`; `func (s *Store) VisibleAgentByHandle(ctx context.Context, viewer, owner AccountID, handle string) (Account, error)` in `go/internal/store/accounts.go`, `ErrNotFound` naming the handle on a miss, same template as `AgentByHandle`.
- `Comms.OpenDM` signature unchanged; `OpenDMAsAccount` and `autoOpenSpawnDM` inherit the change.
- `func newPeerCmd() *cobra.Command`; `func runPeerApprove(ctx context.Context, client compassv1connect.CommsServiceClient, handle string, out io.Writer) error`, `runPeerRevoke` (same shape), `func runPeerList(ctx context.Context, client compassv1connect.CommsServiceClient, out io.Writer) error`.
- `openDmParameters`, `dmParameters` (arktype schemas) in `packages/compass-agent/src/comms.ts` `[compass-agent]`: description strings only; the wire is unchanged.
- Tests (`dm_open_pgtest_test.go` extends; new `peering_pgtest_test.go`): cross-owner open with a live peering creates a `kind=DM` channel named `dm:<idLo>:<idHi>` in the lower owner's `__dm__` group with both agents and both owners as members and `created=true`; B opening on A resumes the same channel; same-owner open still names `dm:<lo>:<hi>` over handles and homes in the caller's owner's group; one-sided approval is `CodeNotFound` with a message byte-identical (after redacting the submitted handle) to the unknown-handle message, keeping `TestOpenDMCrossOwnerIsIndistinguishableNotFound` green with the "not peered" precondition; a same-owner sibling open from an agent caller with no shared channel succeeds (the own-fleet widening reaching `OpenDM`); revoke then open on a third agent is `CodeNotFound` while the existing DM stays listable by both parties; revoke unsubscribes the revoked owner's agent in a shared non-DM channel and a post after revoke reaches no session of that agent (`SubscribedAgents` no longer returns it), while a post into the existing DM still delivers; the rename/reclaim leg: peer A1↔B, open the cross-owner DM, rename A1's handle away by direct `UPDATE`, `CreateUser` a new account under the freed handle, create an agent with the same handle under it, peer it with B, open on the same agent handles is `created=true` and the old channel's member set is unchanged; approve from an agent actor is `CodePermissionDenied`; self-approve is `CodeInvalidArgument`; approve of an agent handle is `CodeNotFound`; `ListPeers` shows the three states. `admin_gate_test.go` rows above. CLI: `peer_test.go` with the `fakeComms`/`startFakeCommsServer` pattern from `message_test.go` asserting each verb's request and the bearer header; `TestNoTokenFlag` (`cli_test.go`) still sweeps the tree. `bun test` in `packages/compass-agent`: wire-shape asserts unchanged.

### T3 — delivery and UI: owner-qualified mentions

`mentionRE` (`go/internal/delivery/consumer.go`) becomes `(?i)@([a-z0-9][a-z0-9._-]*(?:/[a-z0-9][a-z0-9._-]*)?)`. `resolveMentioned` (`go/internal/delivery/dispatch.go`) parses each non-reserved handle with `store.ParseQualifiedHandle`; a bare handle resolves as today under `authorOwner`, a qualified one resolves the owner with `DeliveryReads.UserByHandle` (new on the interface) then `AgentByHandle(ownerID, handle)`. A miss on either step is the existing no-op. The membership check (`memberSet`) is unchanged and is what gates a peered agent; this resolution is not viewer-scoped, and it does not need to be, because a non-member mention is a silent no-op either way (consistent with DL-269). `parseMentions` dedup is on the full qualified string. An email-like `@host/path` in prose now matches one extra segment; server-side it is a no-op miss.

`[compass-ui]`: `MENTION_RE` (`apps/ui/src/comms.ts`) gains the same optional `/segment` group. `byHandle` in `TopicView.tsx` is keyed on both the bare handle and, for an agent, `<ownerHandle>/<handle>`, the owner handle found through `byId.get(a.ownerUserId)`; `mentionRuns` (`apps/ui/src/markdown/mention-runs.ts`) and `rehypeMentionChips` need no change because they only call `byHandle.has`.

Interfaces:

- `DeliveryReads` (`go/internal/delivery/consumer.go`) gains `UserByHandle(ctx context.Context, handle string) (store.Account, error)`; `*store.Store` already implements it; `fakeReads` (`helpers_test.go`) gains the method over a `users map[string]store.Account`.
- `mentionRE` regexp; `func parseMentions(text string) []string` returns qualified strings verbatim; `resolveMentioned` signature unchanged.
- Tests (`mention_test.go` extends): `@bob/x` parses as one token `bob/x`; `@bob/` and `@/x` do not match a qualified form; a qualified mention of a peered member steers it; a qualified mention of a non-member is a no-op; a bare mention still resolves in the author's owner namespace. UI: `comms.test.ts` parse case for the qualified form; `MarkdownText.test.tsx` renders `@bob/x` as a known chip when `byHandle` carries the qualified key and as `unknown` when it does not.

### T4 — e2e: peer, DM, revoke

Extend `TestCommsTenantVisibilityTransport` (`go/e2e/legcomms_tenant_test.go`) or add a sibling test in the same file: after assertion 3 (cross-owner OpenDM is `NOT_FOUND`, now with the owners unpeered), owner 1 approves owner 2 (still `NOT_FOUND`), owner 2 approves owner 1, agent 1 opens a DM on `owner2/agent2` and posts, agent 2 lists the DM and reads the message; owner 1 adds `owner2/agent2` to a shared channel subscribed, posts, agent 2 receives; owner 1 revokes, a fresh open on a third agent of owner 2 is `NOT_FOUND` with the message redacted-identical to the unknown arm, and a further post into the shared channel does not reach agent 2 while the DM still does. The rename/reclaim leg stays a T2 pgtest because no public rename RPC exists to drive it.

Interfaces:

- Generated `CommsServiceClient` for `ApprovePeer`/`RevokePeer`/`ListPeers`; existing `OpenDM`/`PostMessage`/`ListMessages`/`UpdateChannelMembers` clients.
- Tests: the e2e itself (`podmanUsable` gate as today).

## Tasks

- [ ] T1 — `user_peers` + `user_accounts` composite-FK target in `0001_init.sql` (+ `tenant_tables`), `queries/user_peers.sql`, `Store.ApprovePeer`/`RevokePeer`/`ListPeerings`; own-fleet widening in all four predicate copies; peered disjunct in the two resolver copies; header rule + parity test; pgtests (rename-safe, reclaim-safe, cross-tenant id rejected, address-only, revoke unsubscribes)
- [ ] T2 — proto `ApprovePeer`/`RevokePeer`/`ListPeers` + `Peering`/`PeeringState`; same-owner prose swept (five sites); `classifyProcedure` + `admin_gate_test` rows; `Comms.OpenDM` on the viewer-scoped resolver with id-named cross-owner DMs; `peering.go` handlers; `compass peer approve|revoke|list`; agent copy; pgtests + CLI tests + `bun test`
- [ ] T3 — `mentionRE` owner-qualified form, `DeliveryReads.UserByHandle` + fake, `resolveMentioned` qualified resolution; UI `MENTION_RE` + `byHandle` owner/handle keys + tests
- [ ] T4 — e2e: unpeered `NOT_FOUND` → one-sided `NOT_FOUND` → mutual DM round-trip → shared-channel deliver → revoke `NOT_FOUND` + delivery stops

## Open Questions

Five forks are load-bearing. Each is designed against its recommendation above; a different ruling changes the named tasks only.

### OQ-1 (load-bearing) — Edge shape, and whether a recipient can decline

Shape options:

1. **Two directed rows, mutual when both exist** (recommended; §The edge). Writes are one insert or one delete; state is derived; revoke is your own row; a reclaim has no rows. Cost: the resolver disjunct joins two rows, and `ListPeers` is a `FULL OUTER JOIN`.
2. **One symmetric row with a canonical `(lo, hi)` pair and a `state` column.** One row to read per pair; but every write orders the pair first, accept/revoke/re-request are transitions on one row, and "B revoked but A still consents" needs a second column or a fourth state.
3. **Invite token.** A minted, expiring token B redeems. Needs a token table and a redeem RPC on top of either 1 or 2, and cannot express "B asked first". Fits later as a way to create the second row.

Decline/block options (option 1 has no recipient-owned row, so a request sits as `PENDING_INCOMING` until the requester revokes; any user can plant one in any other user's list):

1. **Accept the gap for now** (recommended). A pending row grants nothing and pushes nothing (no stream event in this record), so the cost is one line on the recipient's own `ListPeers`. Named in DL-372 with the follow-up.
2. **Recipient-owned negative row**: `user_peers.decision IN (approve, block)`, or a sibling `user_peer_blocks`. Block hides the request, and makes the edge false regardless of the other row. Additive later; not needed to ship.
3. **Shape option 2 with `declined`/`blocked` as ordinary states.** Free with a state column; costs the write-ordering and the fourth-state problem above.

Recommendation: shape 1, decline 1. Under all shapes the edge keys on `user_accounts.account_id`, so rename and reclaim behave as §The edge states.

Decision needed: confirm shape 1 and decline 1, or pick shape 2 (which gives decline 3 for free).

### OQ-2 (load-bearing) — What a peering grants

(a) Which agents:

1. **All agents of both owners** (recommended; §What a peering grants). No scope column.
2. **Managers only** (agents whose `agent_accounts.role` is the manager role). Smaller blast radius, but the role column is free-text set at spawn and is not a security boundary today; gating on it would make it one.
3. **Per-edge scope list** (`user_peers.scope` naming agents or roles). Most flexible, but a schema and RPC surface for a case Matt named as a follow-up, and an unused column until it exists.

(b) Address-only or enumerate-and-address:

1. **Address-only** (recommended). The peered disjunct goes into the two resolver copies of the predicate (`GetVisibleGlobalHandleID`, `GetVisibleAgentHandleID`) and not the two list copies (`ListVisibleAccounts`, `AccountVisibleTo`). A peered agent can be named in any handle-typed field but does not appear in `ListAccounts`, cannot be a roster vantage, and emits no `AccountChanged` to the peer. Cost: the `accounts.sql` header rule "four copies identical" becomes "two pairs identical, resolver = list + the peered disjunct", pinned by a parity test. The exception exists because naming and listing are different grants; the list pair (the roster-versus-`ListAccounts` anti-drift rule) stays identical.
2. **Enumerate and address.** One disjunct in all four copies. Simpler edit, but one approval exposes the whole foreign fleet: tree topology through `accountToWire`, presence and activity through the roster, a live `AccountChanged` feed, and any of its agents force-subscribable into the approver's channels. Wider than "reachability" and hard to narrow later without a breaking change.
3. **Address-only plus receiving-side subscribe consent**: a peered agent added to a foreign channel lands unsubscribed and only its own owner may subscribe it (a rule in `UpdateChannelMembers` keyed on `expandOwnerMembership`'s owner). Safer for push-into-session, but it makes the shared-channel case a two-step handshake per channel. Fits as a follow-up if push turns out to be the problem.

Recommendation: (a) 1, (b) 1.

Decision needed: confirm (a) 1 and (b) 1; or pick (b) 2 and accept the enumeration in DL-373; or add (b) 3 now.

### OQ-3 (load-bearing) — Error a caller sees with no edge

Options:

1. **Merged `not_found`** for none, one-sided, and revoked (recommended; §Error contract). Keeps DL-269 and the existing tests. A pending request is visible only on `ListPeers`.
2. **Distinct `permission_denied`.** Tells the caller the target exists under someone; an existence oracle on a guessable handle.
3. **`not_found` for strangers, a distinct error once a request is pending.** Leaks the request state to an agent of the other fleet, which is exactly the fleet the human has not yet approved.

Recommendation: option 1.

Decision needed: confirm option 1.

### OQ-4 (load-bearing) — Cross-owner DM key shape

The DM name is the resume key (`GetDMChannelByName` under `UpsertDMChannelTx`, and `verifyReconcileDMTx` adds members to a hit). Options:

1. **Agent account ids: `dm:<agentIdLo>:<agentIdHi>`** (recommended; §Cross-owner DM name and home). Globally unique, never reused, rename-proof and reclaim-proof, no owner-handle reads. Cost: an opaque name on a name-addressed surface; the peer-DM record already calls DMs machine-named, and no surface labels a DM by its name.
2. **Owner id + agent handle: `dm:<ownerIdLo>/<agentLo>:<ownerIdHi>/<agentHi>`.** Closes the owner-reclaim hole; still resumes wrongly after a per-owner agent-handle reclaim (the same hole same-owner `dm:<lo>:<hi>` has today under DL-294, out of scope here).
3. **Owner-handle name for display, resume keyed on a stored `(lo_agent_id, hi_agent_id)` pair.** Readable name and safe resume, but a new lookup column or table on `channels` and a change to the DM upsert. Largest change.

Recommendation: option 1.

Decision needed: confirm option 1, or pick 3 and accept the upsert change.

### OQ-5 (load-bearing) — Revoke semantics for existing channels

`requireDMTwoParties` floors only DMs; an ordinary channel keeps delivering to a subscribed foreign agent after a revoke. Options:

1. **Unsubscribe on revoke, non-DM channels** (recommended; §Revoke). `Store.RevokePeer` flips `subscribed = FALSE` for every agent of the revoked owner in every non-DM channel the revoker's fleet is in, same transaction; membership and history stay; DMs untouched; the edge publishes `ChannelChanged` per touched channel. Limit: a mandatory-subscription channel delivers regardless of the flag, so removal there stays manual.
2. **Remove on revoke.** Remove the foreign agents and the pulled-in owner from non-DM channels. Stops delivery everywhere including mandatory channels, but deletes membership and makes re-peering a re-add; `removeMember` also refuses to remove an owner while an owned agent remains, so ordering matters.
3. **No cascade; report instead.** Keep addressing-only semantics and have `RevokePeerResponse` list the channels still shared so the human can sweep. Least surprising to the store, most work for the human, and delivery continues until they act.

Recommendation: option 1.

Decision needed: confirm option 1, or pick 2 and accept the deletes.

### OQ-6 (non-load-bearing, deferred) — Peering UI and stream event

`ListPeers` and the `compass peer` CLI are the only surfaces in this record. A UI record adds the rendering and, with it, a `PeeringChanged` stream arm at the next free `SubscribeCommsResponse` tag, since the UI is its first reader. Nothing here blocks on it.

## Ledger delta

Rows appended to `docs/designs/DECISIONS.md` in this PR, and the existing rows each one amends:

- **DL-372 (edge shape):** Owner peering is a bilateral trust edge between two user accounts of one tenant, stored as two directed rows in `user_peers(user_id, peer_user_id)`, live only when both directions exist; approve inserts your row, revoke deletes it, state is derived and never stored. The edge keys on `account_id`, never on a handle, so rename leaves it intact and a reclaimed handle starts with no edges. A recipient cannot decline a request in this shape; a pending row grants nothing, and a recipient-owned `decision` column is the additive follow-up. Amends nothing; it is the edge DL-271 names as "the owner-peering authorization edge (RIG-2796)".
- **DL-373 (grant):** A live peering lets each owner and that owner's agents name the other owner's agents, through one new disjunct in the two resolver copies of the D9 account-visibility predicate; the two list copies are unchanged, so a peered agent is not enumerable, not a roster vantage, and emits no `AccountChanged` to the peer until it is a channel co-member. `OpenDM` resolves its peer through the same resolver and carries no owner check. It covers peer-DM open, member fields, and owner-qualified mentions in a shared channel. It never grants spawn, despawn, reparent, parent naming, listing, or roster vantage. The `accounts.sql` parity rule becomes: list pair identical, resolver pair identical, resolver = list + the peered disjunct. **Amends DL-297**: the "same-owner for MVP" scope becomes "same owner, or peered owners"; DL-297's "cross-owner DMs are deferred to the bilateral owner-peering authz edge" clause is discharged by this row. DL-297 flips to `Superseded by DL-373`. Also lands the own-fleet widening the handle-cutover record filed as a follow-on: an agent viewer sees every agent of its own owner.
- **DL-374 (error contract):** No edge, a one-sided edge, and a revoked edge are all the merged in-band `not_found` naming the submitted handle, byte-identical to an unknown handle; a pending request is visible only on the requester's or recipient's own `ListPeers`. **Amends DL-297's** "a cross-owner peer handle is the merged in-band `not_found`" clause (kept, with "unpeered" in place of "cross-owner") and **extends DL-269's** oracle invariant to the peering state.
- **DL-375 (cross-owner DM name and home):** A cross-owner peer-DM is named `dm:<agentIdLo>:<agentIdHi>` over the two agent account ids, never over a handle, and homed in the lower owner id's reserved `__dm__` group; the other owner reaches it by membership. Same-owner DMs keep `dm:<lo>:<hi>` over bare agent handles. A DM name is a resume key, so it never carries a reclaimable token. **Refines DL-296** (members stay "both agents + pulled-in owner(s)", now always two owners on a cross-owner DM) and amends the peer-DM record's Global Constraints naming invariant (`dm--<handleLo>--<handleHi>`, shipped as `dm:<lo>:<hi>`) with the id form for the cross-owner case. DL-296 stays Active.
- **DL-376 (revoke semantics):** Revoking a peering stops new addressing and, in the same transaction, unsubscribes every agent of the revoked owner from every non-DM channel the revoker's fleet is a member of; membership, history, and DMs are untouched; a mandatory-subscription channel keeps delivering until a member removes them. Amends nothing; it is new policy beside DL-296's DM two-party floor.
