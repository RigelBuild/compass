# Collapse the channel-group tree into the agent tree

Tracker: RIG-2684 (follow-up A of RIG-2673)

## Problem

Compass keeps two hierarchies. The agent tree (`agent_accounts.parent_agent_id`) is the reporting spine. The channel-group tree (`channel_groups.parent_group_id`) is a second tree that nothing keeps in step with the first. The schema says so in a comment on `agent_accounts.parent_agent_id` in `go/internal/store/migrations/0001_init.sql`: "NULL = a root agent (mirrors channel_groups.parent_group_id)".

The channels-in-agent-tree record (RIG-1622, `docs/designs/ui/compass-channels-in-agent-tree/design.md`) already moved user channels onto the agent tree. It added `channels.parent_agent_id`, the TREE membership mode, and the `channels_group_xor_agent` CHECK. It kept `ChannelGroup` on purpose, as "server machinery only" (§1 *What replaces `ChannelGroup`*). This record decides what is left: whether `channel_groups` can go, and what takes over each job a group still does.

## Approach

Delete `channel_groups` and every group surface. Each job a group still does moves to a column on `channels` or to the agent tree. Each decision below names the code it replaces.

### D1. One tree: `channel_groups` goes

After RIG-1622 the group tree has no organizing job left. The sidebar is the agent tree (§5 of the prior record). The `New folder` button is gone (DL-397). A group survives only as a container for three server-written namespaces and for SHARED spaces. D2 and D3 give those a home on `channels`, so the table, its nesting, and its lattice have no remaining reader.

Removed: the `channel_groups` table, its trigger `channel_groups_fill_namespace`, its indexes, its RLS `tenant_tables` entry, `channels.group_id`, `channels_group_idx`, `channels_group_name_key`, and the `channels_group_xor_agent` CHECK. The comment on `agent_accounts.parent_agent_id` that says "mirrors channel_groups.parent_group_id" goes with it. Removed with them: the `CreateChannelGroup` and `ListChannelGroups` RPCs, `ChannelGroupChanged`, the agent `create_channel_group` arm, and `KindChannelGroupChanged` in `go/internal/fabric/eventref.go`.

The visibility lattice ("MIN over ancestry") is the `ancestry` and `effective` CTEs. They exist only to compute a group's effective visibility. Every channel copy uses them for one disjunct, the SHARED-group arm:

```sql
OR (
    c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
    )
)
```

That arm is in `ListChannels`, `ChannelVisibleTo`, `OwnerSetLostChannelVisibility`, and `ChannelsByNameForViewer` in `go/internal/store/queries/channels.sql`. D3 replaces it with one column test. The agent tree already carries the other visibility class: DL-345's owner-set arm (`c.parent_agent_id IS NOT NULL AND <anchor owner> IN viewer`). That arm stays as it is. The agent tree needs no MIN walk, because an owner's tree has a single visibility class. So the lattice is not moved to the agent tree. It is deleted, and the owner-set grant already covers the agent tree.

### D2. Reserved homes become a system namespace on `channels`

Three server-written homes live in groups today. Each one is found by owner and a fixed name, and each channel inside it is found by `(group_id, name)`:

| Home | Writer | Channel name | Group lookup |
|---|---|---|---|
| `__dm__` | `EnsureOwnerDMGroupTx` in `store/dm.go` | `dm:<lo>:<hi>`, `xdm:<idLo>:<idHi>` | `GetOwnerDMGroup` |
| `__coordination__` | `EnsureOwnerCoordinationGroupTx` in `store/coordination.go` | `<handle>-coordination`, suffixed `-2`, `-3`… | `GetCoordinationGroup` |
| `__linear__` | `ensureLinearRoutingGroupTx` in `store/linear_routing.go` | `routing` | `GetLinearRoutingGroup` |

The group adds nothing that two columns cannot hold. Add `channels.system_namespace` (0 none, 1 DM, 2 coordination, 3 Linear routing) and `channels.namespace_owner_id` (the owning user). A partial unique index `(namespace_owner_id, system_namespace, name) WHERE system_namespace <> 0` replaces `channels_group_name_key` for these rows. The upserts keep their `ON CONFLICT … DO NOTHING` shape. Only the conflict target changes, so the "never poisons the outer tx" property in `UpsertCoordinationChannelTx` still holds by construction.

This also removes a class of defense. Today a caller could plant a top-level group named `__dm__` at SHARED visibility, so every get-half is "VISIBILITY-DISCRIMINATED" (`GetOwnerDMGroup` filters `visibility = $3`), and `CreateChannel` refuses a reserved group through `isReservedGroupTx`. With the namespace on the channel, no RPC input can write `system_namespace`. `NewChannel` has no such field and no proto field maps to it. So the squat path is gone by construction. `isReservedGroupTx`, `isReservedGroupName`, `GetGroupNameVisibility`, and the visibility discriminators are deleted. The DM resume belt (`verifyReconcileDMTx`) and the Linear wrong-kind refusal (`linearRoutingChannelID`) stay. They defend against drift, not against the group plant.

What does not change:

- The advisory locks keep their key domains: `LockOwnerDM` (`'dm:'`), `LockOwnerCoordination` (`'coordination:'`), and `LockLinearRouting` (`'linear-routing:'`).
- Names: `dmChannelName`, `crossOwnerDMName`, and the coordination suffix loop. The suffix loop still matters, because a reclaimed handle can meet the old manager's channel with a different `owner_account_id`.
- Cross-owner homing (DL-395): an `xdm:` channel's `namespace_owner_id` is the lower owner id.
- DM convert (DL-296): `ConvertDMChannel` now clears `system_namespace` and `namespace_owner_id` instead of `group_id`. The channel leaves the reserved namespace and frees its name, as before.
- Visibility: a system channel gets no non-member grant. A row in an OWNER group got none either, because the channel predicates grant non-members nothing through an OWNER group. Readers stay exactly the members.

A CHECK keeps system channels off the tree and out of the shared set: `system_namespace = 0 OR (parent_agent_id IS NULL AND NOT shared)`. Another CHECK ties the two columns together: `(system_namespace = 0) = (namespace_owner_id IS NULL)`.

### D3. SHARED spaces become a `shared` flag on a root channel

No server code mints a SHARED group. The producers are the `CreateChannelGroup` RPC (caller-chosen visibility), the agent tool `comms_create_channel_group` (`visibility: "shared"`), and the e2e helper `createSharedGroup` in `go/e2e/fixture.go`. Matt ruled that shared spaces "never hang in a tree" and stay at the tree root (DL-397). A flat flag keeps that ruling. It drops only the folder that held the channels.

Add `channels.shared BOOLEAN NOT NULL DEFAULT FALSE`. A shared channel is a root channel: `CHECK (NOT shared OR (parent_agent_id IS NULL AND kind = 0))`. This CHECK takes over the job DL-397 gave `channels_group_xor_agent`: user trees never cross. Shared names are unique per tenant through `channels_shared_name_key ON channels (tenant_id, name) WHERE shared`, the flat analogue of a SHARED group's `(group_id, name)` key. Root non-shared channels stay unkeyed, as today. `ChannelByNameForViewer` still resolves ambiguity by participant and then refuses with "address it by id".

The four channel predicates replace the SHARED-group arm with `OR (c.kind = 0 AND c.shared)`. The copies stay textually identical, per the header rule in `channels.sql`. `ReparentChannel` refuses a shared channel where it refused a grouped one (`row.GroupID.Valid` today). The subscribe filter needs no change for channels: `ChannelVisibleTo` already gates `ChannelChanged`.

### D4. Agents organize channels through the agent tree

Matt ruled "B then A" on RIG-2673 (2026-08-25). B gave agents `comms_create_channel_group`. A, this record, removes groups, so the arm loses its object. Its replacement is the tree. `comms_create_channel` drops `group?` and gains `parent?` (an agent handle, sent as the existing `CreateChannelRequest.parent_agent_handle = 5`) and `shared?`. An agent that wanted a group for a project now attaches the project channel under the manager agent that owns the project. The org-mgmt record never minted its `create_channel_group` row (its "Ledger delta" defers rows to freeze), so no ledger row flips. This record states the reversal so the next reader does not rebuild the arm.

### D5. Wire: remove, do not reserve

DL-186 (Active, Matt, 2026-08-07) strips pre-dogfood wire compat: "all `reserved` markers removed across compass/v1… safe because zero clients are deployed and nothing on disk is proto-encoded." That still holds. Postgres is the only store, and `EventRef` in `go/internal/fabric/eventref.go` is JSON `{kind, row id, tenant}`, never a payload copy. So removed fields are deleted without `reserved`, as `comms.proto` already does for F9 ("REMOVED, not reserved (F9: pre-dogfood, zero stored payloads); field 2 is reused as topic_id").

- `Channel.group_id = 3` becomes `bool shared = 3`. The number is reused, as F9 did.
- `CreateChannelRequest.group_id = 2` becomes `bool shared = 2`. `group_name = 7` is the last field and is deleted.
- `SubscribeCommsResponse.channel_group_changed = 13`, `CommsCallRequest.create_channel_group = 9`, and `CommsCallResult.create_channel_group = 10` are deleted. Their numbers are left as holes, with no renumbering. Open PRs add arms 13–17 to the same oneofs (#2078, #2139, #2153), and renumbering would collide with each of them (OQ4).
- Deleted messages: `ChannelGroup`, `ChannelGroupVisibility`, `ChannelGroupChanged`, `CreateChannelGroupRequest`/`Response`, and `ListChannelGroupsRequest`/`Response`.

### D6. Migration: edit `0001_init.sql` in place

`0001_init.sql` is the only migration file on main, and recent schema changes edit it (#2004 did). RIG-4927 is open: Matt is choosing whether the mattfw dev DB is wiped after each migration stack or once after the last one. This record follows that policy. It edits 0001 in place and adds no migration file. A deployed DB needs a recreate under the RIG-4927 plan; the migrator refuses an edited applied v1 ("applied migration v1 (0001_init.sql) was edited after it ran"). No backfill is needed. DL-397 already ruled "no data migration, with dev-instance channels and groups cleaned up by hand", and nothing durable lives on dev. No tenant table is added. `tenant_tables` only loses `'channel_groups'`.

### D7. What this supersedes and keeps

From the prior record (`docs/designs/ui/compass-channels-in-agent-tree/design.md`):

- **Superseded:** §1 *What replaces `ChannelGroup`* ("`ChannelGroup` is NOT deleted"). The Global Constraints "Additive wire change only. No proto field is removed or renumbered" and "Reserved namespaces are untouchable." DL-397's clause "the `channels_group_xor_agent` CHECK is permanent". `channels_shared_root_only` replaces the CHECK and keeps the ruling's substance.
- **Kept:** §2's owner-set grant (DL-345), §3's membership modes and participant probe (DL-344, except its last clause), §4's no-data-migration posture, §5's single tree, DL-397's other rulings, and DL-416 (TREE re-entry replay).

Ledger rows. New rows: DL-487 (D1), DL-488 (D3), DL-489 (D2), DL-490 (D4, D5). All are `Active (proposed, pending Matt, 2026-10-11)`.

| Row | Action | Why |
|---|---|---|
| DL-344 | Superseded by DL-487 | Its last clause keeps `ChannelGroup`. DL-487 keeps the rest of DL-344. |
| DL-397 | Superseded by DL-488 | It names `channels_group_xor_agent` permanent. DL-488 restates its rulings with the new CHECK. |
| DL-295 | Superseded by DL-489 | Its guard is "`CreateChannel` refuses any create targeting it". D2 makes the guard structural. |
| DL-395 | Superseded by DL-489 | It homes `xdm:` "in the lower owner id's `__dm__` group". DL-489 keeps the name and the lower-owner rule. |
| DL-294, DL-296 | Kept, refined | Their "reserved DM group" now means the DM system namespace. The rest stands. DL-395 refined DL-294 the same way. |
| DL-345 | Kept | The owner-set decision stands. Its query citations describe the pre-change code. |
| DL-136, DL-190 | Kept | Neither row's text names a group. The coordination home ("always provisioned GROUPED") is record prose, not a row. |
| DL-135 | Kept | Roster scopes already walk the agent tree. |

## Alternatives considered

- **Keep a flat reserved-namespace table** (`channel_groups` minus `parent_group_id` and the lattice). This is a smaller change. But it keeps a second container type for three server-only homes, and it keeps every squat defense (visibility discriminators, `isReservedGroupTx`). Two columns on `channels` do the same job with no plant path. See OQ1.
- **Hang reserved homes on the agent tree.** For example, coordination under the manager (`parent_agent_id`) as EXPLICIT. This fails for two of the three homes. A DM has two agent parties and no natural anchor. The anchor's owner set (DL-345) would also get read access to a private pair DM, and a cross-owner DM has two owners. The Linear routing channel belongs to the admin user, not to an agent. The tree is the wrong axis for system homes.
- **Keep SHARED groups as the one surviving group use.** This keeps the lattice, the group RPCs, and the UI group headers for one feature that no server path mints. See OQ2.
- **Deprecate the wire instead of removing it** (`[deprecated = true]`, as `CHANNEL_KIND_GROUP_DM` did). DL-186 makes removal safe pre-dogfood. A deprecated RPC with no store behind it is dead weight. See OQ3.

## Plan

### Global Constraints

- Public repo. Name no private repo or path inside one.
- Migration policy: edit `go/internal/store/migrations/0001_init.sql` in place (RIG-4927). Add no migration file. No backfill.
- After any query or schema edit, run `sqlc generate` from `go/`. `compass-go:sqlc-drift` must pass.
- After any proto edit, run `moon run compass-proto:gen`. `compass-proto:drift` must pass. DL-186 holds: remove fields and add no `reserved`.
- Do not renumber any surviving field or oneof arm. Leave holes at `SubscribeCommsResponse` 13 and `CommsCallRequest`/`CommsCallResult` 9/10 (OQ4).
- The four channel-visibility predicates in `channels.sql` stay textually identical (the file header rule).
- Advisory lock keys keep their domains: `'dm:'`, `'coordination:'`, `'linear-routing:'`.
- No RPC input may set `system_namespace` or `namespace_owner_id`. `NewChannel` gets no such field.
- Error contract: a shared-name collision is `ErrConflict`. Reparenting a shared channel is `ErrInvalidArgument`.

### T1. Schema and store (lands in one PR with T2, because the generated code moves together)

Files: `0001_init.sql`, `queries/{channels,accounts,authz,coordination,dm,linear_routing}.sql`, `store/{channels,coordination,dm,linear_routing,authz,types,inputs}.go`, and their tests.

Interfaces:

```sql
-- channels: drop group_id, channels_group_xor_agent, channels_group_idx, channels_group_name_key
shared              BOOLEAN NOT NULL DEFAULT FALSE,
system_namespace    SMALLINT NOT NULL DEFAULT 0 CHECK (system_namespace IN (0, 1, 2, 3)),
namespace_owner_id  TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
CONSTRAINT channels_shared_root_only CHECK (NOT shared OR (parent_agent_id IS NULL AND kind = 0)),
CONSTRAINT channels_system_namespace_owner CHECK ((system_namespace = 0) = (namespace_owner_id IS NULL)),
CONSTRAINT channels_system_off_tree CHECK (system_namespace = 0 OR (parent_agent_id IS NULL AND NOT shared))

CREATE UNIQUE INDEX channels_system_name_key
    ON channels (namespace_owner_id, system_namespace, name) WHERE system_namespace <> 0;
CREATE UNIQUE INDEX channels_shared_name_key
    ON channels (tenant_id, name) WHERE shared;
```

- Delete the `channel_groups` block, its trigger function, and `'channel_groups'` from `tenant_tables`. Change the `agent_accounts.parent_agent_id` comment to "NULL = a root agent".
- The four predicates replace the SHARED-group arm with `OR (c.kind = 0 AND c.shared)` and drop the `ancestry`/`effective` CTEs. `ListChannels`, `ChannelsByNameForViewer`, and `GetChannel` select `c.shared` in place of `group_id`.
- Delete queries: `InsertChannelGroup`, `GetChannelGroupVisibility`, `ListChannelGroups`, `ChannelGroupVisibleTo`, `GroupCreateAuthorized`, `GetOwnerDMGroup`, `InsertOwnerDMGroup`, `GetCoordinationGroup`, `InsertCoordinationGroup`, `GetLinearRoutingGroup`, `InsertLinearRoutingGroup`, `GetGroupNameVisibility`.
- Rekey the system lookups and inserts. For `dm.sql`:

```sql
-- name: GetDMChannelByName :one
SELECT id, kind FROM channels
WHERE namespace_owner_id = $1 AND system_namespace = 1 AND name = $2;

-- name: InsertDMChannel :one
INSERT INTO channels (id, name, system_namespace, namespace_owner_id, kind, post_policy, owner_account_id, mandatory_subscription)
VALUES ($1, $2, 1, $3, $4, $5, NULL, $6)
ON CONFLICT (namespace_owner_id, system_namespace, name) WHERE system_namespace <> 0 DO NOTHING
RETURNING id;
```

  Coordination uses `system_namespace = 2`; Linear routing uses 3. `ConvertDMChannel` sets `system_namespace = 0, namespace_owner_id = NULL`. `InsertChannel` and `InsertHomeChannel` lose `group_id`; `InsertChannel` gains `shared`.
- Go signatures:

```go
type SystemNamespace int16 // SystemNamespaceNone=0, DM=1, Coordination=2, LinearRouting=3

type DMChannelSpec struct { NamespaceOwnerID AccountID; Name string; Members []AccountID }
type CoordinationChannelSpec struct { NamespaceOwnerID AccountID; BaseName string; OwnerAccountID AccountID; Policy ChannelPolicy }
type NewChannel struct { /* GroupID removed */ Shared bool; /* other fields unchanged */ }
type Channel struct { /* GroupID removed */ Shared bool; /* other fields unchanged */ }
```

- Delete: `ChannelGroup`, `ChannelGroupID`, `ChannelGroupVisibility`, `NewChannelGroup`, `CreateChannelGroup`, `ListChannelGroups`, `ChannelGroupVisibleTo`, `ChannelGroupByRefForViewer`, `resolveGroupRef`, `groupRefHints`, `requireGroupCreateAuthz`, `isReservedGroupTx`, `isReservedGroupName`, `EnsureOwnerDMGroupTx`, `EnsureOwnerCoordinationGroupTx`, `ensureLinearRoutingGroupTx`.
- `validateNewChannel` refuses `Shared` with a `ParentAgentID` or a non-CHANNEL kind (`ErrInvalidArgument`). `ReparentChannel` refuses `row.Shared`. A `channels_shared_name_key` violation maps to `ErrConflict`.

Tests (each fails before the change):

- A shared channel is visible to a non-member of another owner. A non-shared root channel is not.
- Two shared channels with one name in one tenant: the second create is `ErrConflict`.
- `CreateChannel` with `Shared` and a parent agent is `ErrInvalidArgument`. `ReparentChannel` of a shared channel is `ErrInvalidArgument`.
- Two concurrent `OpenDM` calls for one pair return one channel. DM convert frees the name, and the next open mints a new DM.
- An existing coordination channel with a different `owner_account_id` (a reclaimed handle) makes the reconcile pick the `-2` name. This replaces the user-squat test, which can no longer happen.
- A raw insert of a system channel with `parent_agent_id` set fails `channels_system_off_tree`.
- Delete `channel_groups_reserved_pgtest_test.go` and `group_ref_test.go`. They test removed code.

### T2. Proto and comms service (same PR as T1)

Files: `proto/compass/v1/comms.proto`, `proto/compass/v1/agent_gateway.proto`, the regenerated trees, `go/internal/comms/{comms,agent_caller,mapping,subscribe}.go`, `go/internal/runnerhub/{hub,relay_comms}.go`, `go/internal/auth/admin_gate.go`, `go/internal/fabric/{eventref.go,SUBJECTS.md}`.

Interfaces:

```proto
message Channel {
  // Visible to every account in the tenant. Only a root CHANNEL can be shared.
  bool shared = 3;
  // other fields unchanged
}
message CreateChannelRequest {
  string name = 1;
  bool shared = 2;            // was group_id
  ChannelKind kind = 3;
  repeated string member_handles = 4;
  string parent_agent_handle = 5;
  ChannelMembershipMode membership_mode = 6;
  // group_name = 7 removed
}
```

- Delete the RPCs `CreateChannelGroup` and `ListChannelGroups`, the messages listed in D5, the `channel_group_changed = 13` arm, and the agent arms `create_channel_group = 9` and `= 10`. Rewrite two comments: `AgentAccount.parent_agent_id` loses "Mirrors ChannelGroup.parent_group_id", and `Channel.parent_agent_id`'s "Never set on a SHARED-group channel" becomes "Never set on a shared channel".
- Delete `Comms.CreateChannelGroup`, `Comms.ListChannelGroups`, `CreateChannelGroupAsAccount`, `resolveAgentGroupRef`, `groupToWire`, `groupVisibilityToWire`, `groupVisibilityFromWire`, `publishChannelGroupChanged`, the `ChannelGroupVisibleTo` method of the subscribe visibility interface, `CommsCaller.CreateChannelGroupAsAccount`, the relay case, `KindChannelGroupChanged`, and the two `admin_gate.go` procedure entries.
- `CreateChannelAsAccount` no longer resolves a group. It passes `parent_agent_handle` and `shared` through.

Tests: `org_mgmt_pgtest_test.go` and `relay_org_mgmt_test.go` lose the group cases. They gain one agent create of a shared channel and one agent create under a parent handle. `channel_attach_wire_test.go` pins `CreateChannelRequest.shared` at 2. `relay_arm_coverage_test.go` drops arm 9.

### T3. Agent tools

Files: `packages/compass-agent/src/comms.ts`, `comms.test.ts`, `compassv1.ts`.

Interfaces:

```ts
export const createChannelParameters = type({
  name: /* unchanged */,
  "members?": /* unchanged */,
  "parent?": type("string").narrow(nonBlank).describe("Agent handle to attach the channel under; omit for a root channel"),
  "shared?": type("boolean").describe("Visible to every account; root channels only"),
});
```

- Delete `createChannelGroupParameters` and `commsCreateChannelGroup`, and drop the tool from the returned list. `comms_create_channel` maps `parent` to `parentAgentHandle` and `shared` to `shared`.

Tests: the schema refuses `parent` together with `shared: true`. `comms_create_channel` sends `parentAgentHandle`. The tool list has no `comms_create_channel_group`.

### T4. UI and client

Files: `apps/ui/src/{comms.ts,comms-stub.ts,store.ts}`, `apps/ui/src/live/{adapt,comms-state,stream,comms-fake}.ts`, `apps/ui/src/components/LeftSidebar.tsx`, `packages/compass-client/src/index.ts`, and their tests.

- Drop `ChannelGroup`, `adaptChannelGroup`, the `channelGroups` state, the `listChannelGroups` snapshot read, and the `channelGroupChanged` event case. `adaptChannel` maps `shared`.
- `channelSections` takes channels only. `LeftSidebar.tsx` replaces `sharedGroups()` with one "Shared" section over `channels.filter((c) => c.shared)`.

Tests: `comms-state.test.ts` and `stream.test.ts` drop the group cases. A shared channel renders in the Shared section and under no agent.

### T5. e2e fixture

File: `go/e2e/fixture.go`. Replace `createSharedGroup` with a `CreateChannel` call that sets `shared: true`. Keep each test's cross-owner visibility assertion.

### T6. Docs and citation sweep

- `docs/specs/product/compass.md`: rewrite the channel-group text as "agent tree plus shared root channels".
- Leave the prior record §1 and `docs/designs/agent/compass-manager-comms-substrate/design.md` ("always provisioned GROUPED") as history. The flipped rows point here.
- Code-comment sweep for the flipped ids: `grep -rn "DL-344\|DL-395\|DL-397\|DL-295" go apps packages proto` has no hits on main today. Re-run it after rebase. Remove the comments in `store/coordination.go` that cite "design.md:570-573" (the GROUPED home).
- Done check: `grep -rn "channel_groups\|ChannelGroup\|parent_group_id\|__dm__\|__coordination__\|__linear__" go apps packages proto`, excluding generated trees, returns nothing.

## Tasks

- [ ] T1 + T2: schema, queries, store, proto, comms, runnerhub, auth gate, fabric kind; sqlc and buf regen.
- [ ] T3: agent tools (`comms_create_channel` gains `parent`/`shared`; `comms_create_channel_group` deleted).
- [ ] T4: UI and compass-client drop groups; a Shared section over `shared` channels.
- [ ] T5: e2e fixture creates shared channels.
- [ ] T6: product spec rewrite and citation sweep.
- [ ] Dev DB recreate per RIG-4927 after T1 merges.

## Open Questions

### OQ1. Drop `channel_groups` entirely, or keep a flat reserved-namespace table?

- (a) Drop it. System homes become `channels.system_namespace` + `namespace_owner_id`. No plant path, so the squat guards go. Cost: three upserts are rekeyed.
- (b) Keep a flat `channel_groups` (no `parent_group_id`, no visibility) for the three homes only. The diff is smaller, but a second container type and the squat guards stay.

**Recommendation:** (a). The Plan builds (a). Delta for (b): T1 keeps `channel_groups (id, name, owner_user_id, tenant_id)` with a unique `(owner_user_id, name)` key, keeps `channels.group_id` for system rows only, keeps `EnsureOwner*GroupTx` and `isReservedGroupTx`, and drops only nesting, visibility, and the lattice. DL-295 stays Active.

### OQ2. What replaces SHARED-group grants?

- (a) A `channels.shared` flag on root channels, with a unique name per tenant. It keeps DL-397's "never in a tree".
- (b) Remove SHARED spaces. Cross-owner reach uses membership and peering (DL-395) only. No server path mints a SHARED group today.
- (c) Keep SHARED groups as the only group use. The lattice and the group RPCs stay.

**Recommendation:** (a). It keeps a product affordance Matt ruled on (DL-397) for the cost of one column. Delta for (b): drop `shared`, `channels_shared_*`, and the predicate arm; T3 drops `shared?`; T4 drops the Shared section; T5 uses explicit cross-owner members. Delta for (c): this record shrinks to OQ1, and DL-397 stays Active.

### OQ3. Remove proto `ChannelGroup`, or deprecate it?

- (a) Remove with no `reserved` (DL-186 and the F9 precedent). Reuse `Channel` 3 and `CreateChannelRequest` 2 for `shared`.
- (b) Remove, but add `reserved` lines, as `agent_gateway.proto` still does for `initial_prompt`.
- (c) Mark the surface `[deprecated = true]` and stub the RPCs as `Unimplemented`.

**Recommendation:** (a). Delta for (b): add `shared` at new numbers (`Channel.shared = 13`, `CreateChannelRequest.shared = 8`) and add `reserved` for each removed number and name. Delta for (c): T2 keeps the messages and returns `CodeUnimplemented`; T3 and T4 still drop their callers.

### OQ4. Leave oneof holes, or renumber?

DL-186 says live fields are "densely renumbered (no holes)". Open PRs #2078, #2139 and #2153 add arms 13–17 to the same oneofs, so renumbering now collides with each of them.

- (a) Leave holes at 13 and 9/10 now. Renumber after those PRs land.
- (b) Renumber densely now and rebase the open PRs.

**Recommendation:** (a). It deviates from DL-186's "no holes", so it needs Matt's sign-off. Delta for (b): T2 moves `SubscribeCommsResponse` arms 14+ down by one and `CommsCallRequest`/`CommsCallResult` arms 10+/11+ down by one; each open PR rebases.

### OQ5. Remove the RIG-2673 B arm?

Matt ruled "B then A" (2026-08-25). This record removes B's `comms_create_channel_group`, as A implies. B's ledger row was never minted, so no row flips.

- (a) Remove the arm. `comms_create_channel` gains `parent?` and `shared?` (D4).
- (b) Keep a `comms_create_channel_group` name as an alias that creates a shared root channel.

**Recommendation:** (a). An alias keeps a tool name for a concept that no longer exists. Delta for (b): T3 keeps the tool with `name` and `shared` only, mapped to `CreateChannel`. Either way, the org-mgmt record's DL-new-5 text names "channel groups"; its owner should reword it at freeze.
