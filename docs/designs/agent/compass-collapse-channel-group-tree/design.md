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

Two things change in how the homes are defended.

- **Uniqueness.** The reserved groups have no unique key. `channels_group_name_key` covers channels, but the group index `channel_groups_owner_parent_name_key` exempts the three reserved names (`WHERE NOT (parent_group_id IS NULL AND name IN ('__dm__', '__linear__', '__coordination__'))`). So only the advisory lock stops two seeds from minting two groups. `EnsureLinearRoutingChannel` says so: "The group get-then-insert has no unique key to lean on, so the lock is what keeps racing seeds from minting two groups". `channels_system_name_key` is a real key on the channel row.
- **Squats.** `CreateChannelGroup` already refuses the three reserved names at top level (`if g.ParentGroupID == "" && isReservedGroupName(g.Name)`). So the visibility-discriminated get-halves (`GetOwnerDMGroup` filters `visibility = $3`) only guard against legacy or raw-SQL rows. The one live squat is narrower. `CreateChannel` refuses only the DM and Linear groups (`isReservedGroupTx`: `reservedName := row.Name == dmGroupName || row.Name == linearRoutingGroupName`). A user can therefore create a channel inside their own `__coordination__` group, and the coordination suffix loop absorbs it. With the namespace on the channel, no RPC input can write `system_namespace`: `NewChannel` has no such field, and no proto field maps to it. That path closes by construction. `isReservedGroupTx`, `isReservedGroupName`, `GetGroupNameVisibility`, and the visibility discriminators are deleted. The DM resume belt (`verifyReconcileDMTx`) and the Linear wrong-kind refusal (`linearRoutingChannelID`) stay. They defend against drift.

What does not change:

- Names: `dmChannelName`, `crossOwnerDMName`, and the coordination suffix loop. The suffix loop still matters, because a reclaimed handle can meet the old manager's channel with a different `owner_account_id`.
- Cross-owner homing (DL-395): an `xdm:` channel's `namespace_owner_id` is the lower owner id.
- DM convert (DL-296): `ConvertDMChannel` now clears `system_namespace` and `namespace_owner_id` instead of `group_id`. The channel leaves the reserved namespace and frees its name, as before.
- Visibility: a system channel gets no non-member grant. A row in an OWNER group got none either, because the channel predicates grant non-members nothing through an OWNER group. Readers stay exactly the members.

A CHECK keeps system channels off the tree and out of the shared set: `system_namespace = 0 OR (parent_agent_id IS NULL AND NOT shared)`. Another CHECK ties the two columns together: `(system_namespace = 0) = (namespace_owner_id IS NULL)`. `ReparentChannel` today refuses every grouped channel (`if row.GroupID.Valid`), which covers coordination and Linear channels. After the change it refuses `system_namespace <> 0` with `ErrInvalidArgument`, before the CHECK can fire. Without that refusal, an owner who reparents their coordination channel would hit the CHECK and get an internal error.

### D3. SHARED spaces become a `shared` flag on a root channel

No server code mints a SHARED group. The producers are the `CreateChannelGroup` RPC (caller-chosen visibility), the agent tool `comms_create_channel_group` (`visibility: "shared"`), and the e2e helper `createSharedGroup` in `go/e2e/fixture.go`. Matt ruled that shared spaces "never hang in a tree" and stay at the tree root (DL-397). A flat flag keeps that ruling. It drops only the folder that held the channels.

Add `channels.shared BOOLEAN NOT NULL DEFAULT FALSE`. A shared channel is a root channel: `CHECK (NOT shared OR (parent_agent_id IS NULL AND kind = 0))`. This CHECK takes over the job DL-397 gave `channels_group_xor_agent`: user trees never cross. Shared names are unique per tenant through `channels_shared_name_key ON channels (tenant_id, name) WHERE shared`, the flat analogue of a SHARED group's `(group_id, name)` key. Root non-shared channels stay unkeyed, as today. `ChannelByNameForViewer` still resolves ambiguity by participant and then refuses with "address it by id".

The four channel predicates replace the SHARED-group arm with `OR (c.kind = 0 AND c.shared)`. The header rule in `channels.sql` says the copies "MUST stay textually identical". `TestChannelVisibilityOwnerSetParity` (`store/channel_participant_pgtest_test.go`) enforces it for two of the four copies (`ListChannels`, `ChannelVisibleTo`) on one owner-set fixture. `OwnerSetLostChannelVisibility` already spells the viewer as `cand.account_id`. T1 extends that test to all four copies and to shared and system fixtures. `ReparentChannel` refuses a shared channel where it refused a grouped one. The subscribe filter needs no change for channels: `ChannelVisibleTo` already gates `ChannelChanged`.

### D4. Agents organize channels through the agent tree

Matt ruled B on RIG-2673 (2026-08-25) and directed follow-up A, which is this record. The org-mgmt record (`docs/designs/agent/compass-agent-org-mgmt-tools/design.md`) says "Matt ruled B, 2026-08-25" and "Matt also directed a **follow-up (A): collapse the channel-group tree into the agent tree**". B gave agents `comms_create_channel_group`. A removes groups, so the arm loses its object. Its replacement is the tree. `comms_create_channel` drops `group?` and gains `parent?` (an agent handle, sent as the existing `CreateChannelRequest.parent_agent_handle = 5`) and `shared?`. An agent that wanted a group for a project now attaches the project channel under the manager agent that owns the project. The org-mgmt record never minted its `create_channel_group` row (its "Ledger delta" defers rows to freeze), so no ledger row flips. This record states the reversal so the next reader does not rebuild the arm.

### D5. Wire: remove, do not reserve

DL-186 (Active, Matt, 2026-08-07) strips pre-dogfood wire compat: "all `reserved` markers removed across compass/v1… safe because zero clients are deployed and nothing on disk is proto-encoded." The storage half still holds. Postgres is the only store, and `EventRef` in `go/internal/fabric/eventref.go` is JSON `{kind, row id, tenant}`, never a payload copy. So removed fields are deleted without `reserved`. The agent image pin does not re-arm DL-186, which re-arms "at GA / first pinned client". `guest-image/agent-oci.lock` is an in-repo pin that Renovate bumps with a relock task (`tools/renovate/config.json5`). That makes the agent a client released with this repo, not an external pinned client.

Schema versions can still mix during a rollout. The agent image is pinned by digest apart from the server (`guest-image/agent-oci.lock`, `"tag": "git-7d22c69390cb"`), and an open UI tab keeps its old bundle across a server restart. Deleting a field is safe under mixing: an old decoder reads the absent field as its default, and a new decoder skips the unknown one. Deleting an RPC or a oneof arm costs more, but the cost is bounded and visible. A stale tab's snapshot calls `listChannelGroups` inside `Promise.all` (`apps/ui/src/live/stream.ts`), so every resync fails until the tab reloads. The pinned agent's `comms_create_channel_group` fails in-band until the pin bump. Its `comms_create_channel` with a group creates a root channel instead. Both are accepted pre-dogfood. Reusing a number at a different wire type is worse, because it fails silently. An old decoder would read a `bool` varint at field 3 as a string length and misparse the rest of `Channel`. The F9 precedent does not cover a type change. It reused field 2 at the same type: "field 2 is reused as topic_id, same `string` wire type". So `shared` takes fresh numbers.

- `Channel.group_id = 3` is deleted. `bool shared = 13` is added (the next free number after `membership_mode = 12`).
- `CreateChannelRequest.group_id = 2` and `group_name = 7` are deleted. `bool shared = 8` is added. Field 7 is not reused, because it held a `string`. An old agent that still sends `group_name` gets a root channel; the server skips the unknown field.
- `SubscribeCommsResponse.channel_group_changed = 13`, `CommsCallRequest.create_channel_group = 9`, and `CommsCallResult.create_channel_group = 10` are deleted. Their numbers stay unused. This follows a local precedent: the `trigger_traceparent` comment in `agent_gateway.proto` says "numbers stay in place, renamed never renumbered (OQ-1b)". Arm numbers there skip numbers that message-level fields hold (`ForgeCallRequest` "oneof arms 2-11 and 14-15"; `CommsCallRequest` 10). A number that no field uses is new, so OQ4 asks Matt to confirm it. Renumbering later would break the pinned agent and stale tabs. In `CommsCallRequest` it cannot even be built, because field 10 is the message-level `trigger_traceparent`.
- Deleted messages: `ChannelGroup`, `ChannelGroupVisibility`, `ChannelGroupChanged`, `CreateChannelGroupRequest`/`Response`, and `ListChannelGroupsRequest`/`Response`.

### D6. Migration: edit `0001_init.sql` in place

`0001_init.sql` is the only migration file on main, and recent schema changes edit it (#2004 did). RIG-4927 is open: Matt is choosing whether the mattfw dev DB is wiped after each migration stack or once after the last one. This record follows that policy. It edits 0001 in place and adds no migration file. A deployed DB needs a recreate under the RIG-4927 plan; the migrator refuses an edited applied v1 ("applied migration v1 (0001_init.sql) was edited after it ran"). No backfill is needed. DL-397 already ruled "no data migration, with dev-instance channels and groups cleaned up by hand", and nothing durable lives on dev. No tenant table is added. `tenant_tables` only loses `'channel_groups'`.

### D7. What this supersedes and keeps

From the prior record (`docs/designs/ui/compass-channels-in-agent-tree/design.md`):

- **Superseded:** §1 *What replaces `ChannelGroup`* ("`ChannelGroup` is NOT deleted"). The Global Constraints "Additive wire change only. No proto field is removed or renumbered" and "Reserved namespaces are untouchable." DL-397's clause "the `channels_group_xor_agent` CHECK is permanent". `channels_shared_root_only` replaces the CHECK and keeps the ruling's substance.
- **Kept:** §2's owner-set grant (DL-345), §3's membership modes and participant probe (DL-344, except its last clause), §4's no-data-migration posture, §5's single tree, DL-397's other rulings, and DL-416 (TREE re-entry replay).

Ledger rows. New rows: DL-487 (D1), DL-488 (D3), DL-489 (D2), DL-490 (D4, D5). All are `Active (proposed, pending Matt, 2026-10-11)`. This PR is freeze-gated. The flips take effect for every agent that reads the ledger once it merges, so it must not merge before Matt rules. At ratification, all eight touched statuses are rewritten to `(Matt, <date>)`.

| Row | Action | Why |
|---|---|---|
| DL-344 | Clause superseded by DL-487 | Only "`ChannelGroup` is NOT deleted" changes. Clause form as in DL-097. |
| DL-397 | Clause superseded by DL-488 | Only "the `channels_group_xor_agent` CHECK is permanent" changes. Its other rulings stay Active. |
| DL-295 | Superseded by DL-489 | Its group guard becomes structural. |
| DL-395 | Superseded by DL-489 | The home changes from a group to the DM namespace. DL-489 restates the naming rule and its rationale. |
| DL-294, DL-296 | Kept, refined by DL-489 | "Reserved DM group" now means the DM namespace. |
| DL-345 | Kept | DL-487 notes that its SHARED-group arm is now `c.shared`. |
| DL-136, DL-190, DL-135 | Kept | No row text names a group. |

## Alternatives considered

- **Keep a flat reserved-namespace table** (`channel_groups` minus `parent_group_id` and the lattice). This is a smaller change. But it keeps a second container type for three server-only homes, and it keeps the guards that type needs (D2, *Squats*). Two columns on `channels` do the same job, with a real unique key and no RPC that can write them. See OQ1.
- **Hang reserved homes on the agent tree.** For example, coordination under the manager (`parent_agent_id`) as EXPLICIT. This fails for two of the three homes. A DM has two agent parties and no natural anchor. The anchor's owner set (DL-345) would also get read access to a private pair DM, and a cross-owner DM has two owners. The Linear routing channel belongs to the admin user, not to an agent. The tree is the wrong axis for system homes.
- **Keep SHARED groups as the one surviving group use.** This keeps the lattice, the group RPCs, and the UI group headers for one feature that no server path mints. See OQ2.
- **Deprecate the wire instead of removing it** (`[deprecated = true]`, as `CHANNEL_KIND_GROUP_DM` did). DL-186 makes removal safe pre-dogfood. A deprecated RPC with no store behind it is dead weight. See OQ3.
- **Land in two PRs: add, then delete.** PR1 would add `shared` and `system_namespace` and move every caller; PR2 would delete the group surface. Both PRs would do real work, so neither is inert. But PR1 must either keep SHARED groups and the `shared` flag both live, with both arms in all four predicates, or stop the UI reading groups while the `CreateChannelGroup` RPC can still create them. The first path is transitional code that PR2 deletes. The second hides live data for one PR. The plan lands one PR instead (see *Landing*).

## Plan

### Global Constraints

- Public repo. Name no private repo or path inside one.
- Migration policy: edit `go/internal/store/migrations/0001_init.sql` in place (RIG-4927). Add no migration file. No backfill.
- After any query or schema edit, run `sqlc generate` from `go/`. `compass-go:sqlc-drift` must pass.
- After any proto edit, run `moon run compass-proto:gen`. It regenerates all four trees. `compass-proto:drift` must pass. DL-186 holds: remove fields and add no `reserved`.
- Never reuse a field number at a different wire type. New fields take fresh numbers. Re-check the next free number in `Channel` and `CreateChannelRequest` at rebase.
- Never renumber a surviving field or oneof arm. Removed numbers stay unused.
- The four channel-visibility predicates in `channels.sql` keep one shape. The extended `TestChannelVisibilityOwnerSetParity` enforces it.
- Advisory lock keys keep their domains: `'dm:'`, `'coordination:'`, `'linear-routing:'`.
- No RPC input may set `system_namespace` or `namespace_owner_id`. `NewChannel` gets no such field.
- Error contract: a shared-name collision is `ErrConflict`. Reparenting a shared channel or a system channel is `ErrInvalidArgument`.

### Landing

T1–T6 land as one PR, with one commit per task so a reviewer can read it by surface. The commits are review slices, not bisectable: only the PR head builds. No smaller PR can be green. One proto regen deletes `ChannelGroup`, `ChannelGroupVisibility` and `CreateChannelGroupRequestSchema` from the TS trees, and the agent tools, the UI adapter and the client index import them. The e2e fixture (`//go:build podman`) calls `CreateChannelGroupRequest` and `ChannelGroupVisibility_…`. So the store, proto, agent, UI and e2e edits must merge together. The split into add-then-delete is rejected in *Alternatives considered*. Most of the diff is deletion.

### T1. Schema and store

Files: `0001_init.sql`, `queries/{channels,accounts,authz,coordination,dm,linear_routing}.sql`, `store/{channels,coordination,dm,linear_routing,authz,types,inputs}.go`, and these tests: `store/{channels_test,store_test,channel_tree_pgtest_test,channel_by_name_pgtest_test,channel_participant_pgtest_test,coordination_pgtest_test,dm_pgtest_test,linear_routing_pgtest_test,rls_pgtest_test,authz_test}.go`, plus `comms/dm_open_pgtest_test.go`.

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
- `validateNewChannel` refuses `Shared` with a `ParentAgentID` or a non-CHANNEL kind (`ErrInvalidArgument`). `LockChannelForReparent` selects `shared` and `system_namespace` in place of `group_id`. `ReparentChannel` refuses `row.Shared` and `row.SystemNamespace <> 0` with `ErrInvalidArgument`. A `channels_shared_name_key` violation maps to `ErrConflict`.

New-behavior tests (each fails before the change):

- Extend `TestChannelVisibilityOwnerSetParity` to all four copies. Over a fixture of member, shared, owner-set, system and stranger channels, `ListChannels`, `ChannelVisibleTo` and `ChannelsByNameForViewer` agree for every viewer, and `OwnerSetLostChannelVisibility` returns exactly the owner-set accounts the other three deny.
- A shared channel is visible to a non-member of another owner. A non-shared root channel is not.
- Two shared channels with one name in one tenant: the second create is `ErrConflict`.
- `CreateChannel` with `Shared` and a parent agent is `ErrInvalidArgument`. `ReparentChannel` of a shared channel is `ErrInvalidArgument`.
- `ReparentChannel` of the owner's coordination channel is `ErrInvalidArgument`, not an internal error.
- Raw-insert tests of the new constraints. A system channel with `parent_agent_id` set fails `channels_system_off_tree`. A second system row with the same `(namespace_owner_id, system_namespace, name)` fails `channels_system_name_key`. These replace the SHARED `__coordination__` plant in `coordination_pgtest_test.go` and the `JOIN channel_groups` assertions in `dm_pgtest_test.go` and `linear_routing_pgtest_test.go`.

Kept-behavior tests (green before and after, now on the rekeyed queries):

- Two concurrent `OpenDM` calls for one pair return one channel. DM convert frees the name, and the next open mints a new DM.
- An existing coordination channel with a different `owner_account_id` (a reclaimed handle) makes the reconcile pick the `-2` name.
- `TestOpenDMRevokeWaitsForOpenDMCommit` keeps its gate. Its holder `holdDMGroupLock` locks `channel_groups … FOR UPDATE` and relies on "The channel FK takes KEY SHARE". The rework deletes the helper `ensureHostDMGroup` and locks `SELECT 1 FROM user_accounts WHERE account_id = $host FOR UPDATE`, because the new `namespace_owner_id` FK takes KEY SHARE on that row at the channel insert. [INFERENCE] Nothing earlier in `openDMTx` locks that row: the peering check locks `user_peers` rows. A run with the holder removed must fail, which shows the gate is real.
- `TestRestartDurabilityReadsBackFullGraph` in `store_test.go` builds a shared channel in place of the SHARED group `proj`. `channels_test.go` and `channel_tree_pgtest_test.go` drop their `channel_groups` inserts and `GroupID` cases.

Delete `channel_groups_reserved_pgtest_test.go` and `group_ref_test.go`. They test removed code.

### T2. Proto and comms service

Files: `proto/compass/v1/comms.proto`, `proto/compass/v1/agent_gateway.proto`, the regenerated trees, `go/internal/comms/{comms,agent_caller,mapping,subscribe,dm,coordination}.go`, `go/internal/runnerhub/{hub,relay_comms}.go`, `go/internal/auth/admin_gate.go`, `go/internal/fabric/{eventref.go,SUBJECTS.md}`, and these tests: `comms/{org_mgmt_pgtest_test,visibility_filter_test,subscribe_failclosed_test,channel_attach_wire_test,coordination_pgtest_test}.go`, `runnerhub/{relay_org_mgmt_test,relay_arm_coverage_test,helpers_test}.go`, `auth/interceptor_pgtest_test.go`, `fabric/{event_fabric_test,subjects_test}.go`, `go/server/{comms_actor_pgtest_test,linear_responder_pgtest_test}.go`.

Interfaces:

```proto
message Channel {
  // group_id = 3 removed; the number stays unused.
  // fields 1-2 and 4-12 unchanged
  // Visible to every account in the tenant. Only a root CHANNEL can be shared.
  bool shared = 13;
}
message CreateChannelRequest {
  string name = 1;
  // group_id = 2 removed; the number stays unused.
  ChannelKind kind = 3;
  repeated string member_handles = 4;
  string parent_agent_handle = 5;
  ChannelMembershipMode membership_mode = 6;
  // group_name = 7 removed; the number stays unused.
  bool shared = 8;
}
```

- Delete the RPCs `CreateChannelGroup` and `ListChannelGroups`, the messages listed in D5, the `channel_group_changed = 13` arm, and the agent arms `create_channel_group = 9` and `= 10`. Rewrite these comments:
  - `AgentAccount.parent_agent_id` loses "Mirrors ChannelGroup.parent_group_id".
  - `Channel.parent_agent_id`: "Never set on a SHARED-group channel" becomes "Never set on a shared channel".
  - `UpdateChannelMembersRequest.convert_channel_name`: "detaches it from the reserved DM group" becomes "detaches it from the DM namespace".
  - In `agent_gateway.proto`, the `CommsCallRequest` header ("create_channel and create_channel_group use group_name / parent_group_name") and the `CommsCallResult` arm list (`create_channel_group`).
- Delete `Comms.CreateChannelGroup`, `Comms.ListChannelGroups`, `CreateChannelGroupAsAccount`, `resolveAgentGroupRef`, `groupToWire`, `groupVisibilityToWire`, `groupVisibilityFromWire`, `publishChannelGroupChanged`, the `ChannelGroupVisibleTo` method of the subscribe visibility interface, `CommsCaller.CreateChannelGroupAsAccount`, the relay case, `KindChannelGroupChanged`, and the two `admin_gate.go` procedure entries. Remove `create_channel_group` from the relay's "no recognized variant set" error text.
- `CreateChannelAsAccount` no longer resolves a group. It passes `parent_agent_handle` and `shared` through.
- The system homes lose their group step. `openDMTx` (`comms/dm.go`) calls `UpsertDMChannelTx(ctx, tx, store.DMChannelSpec{NamespaceOwnerID: owner, Name: name, Members: members})` after `LockOwnerDMTx`. `reconcileCoordinationTx` (`comms/coordination.go`) builds `store.CoordinationChannelSpec{NamespaceOwnerID: ownerUserID, BaseName: …, OwnerAccountID: …, Policy: …}` after `LockOwnerCoordinationTx`. Rewrite its lock comment ("into two groups or …") to name channels.

Tests:

- `org_mgmt_pgtest_test.go` and `relay_org_mgmt_test.go` lose the group cases. They gain one agent create of a shared channel and one agent create under a parent handle.
- The actor-threading probes in `auth/interceptor_pgtest_test.go` and `go/server/comms_actor_pgtest_test.go` use `CreateChannelGroup` today. They switch to `CreateChannel` and assert that the caller is in `member_account_ids`, since the store makes the actor a founding member.
- `visibility_filter_test.go` drops `TestSubscribeCommsChannelGroupChangedScoping`, and rewrites `TestSubscribeCommsChannelChangedSharedVisibleToNonMember` to build its channel with `CreateChannel{shared: true}`, keeping the non-member-receives and private-leak assertions. `subscribe_failclosed_test.go` drops its `ChannelGroupVisibleTo` fake. `subjects_test.go` drops `KindChannelGroupChanged`.
- `channel_attach_wire_test.go` pins `Channel.shared` at 13 and `CreateChannelRequest.shared` at 8. `relay_arm_coverage_test.go` drops arm 9.

### T3. Agent tools

Files: `packages/compass-agent/src/comms.ts`, `comms.test.ts`, `compassv1.ts`, `packages/compass-agent/AGENTS.md`, `config/skills/comms-playbook/SKILL.md`, and `docs/concepts/tools.md`. The last three name `comms_create_channel_group` or say a channel goes "in a group". Rewrite them for `parent` and `shared`.

Interfaces:

```ts
export const createChannelParameters = type({
  name: /* unchanged */,
  "members?": /* unchanged */,
  "parent?": type("string")
    .narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
    .describe("Agent handle to attach the channel under (must not be blank); omit for a root channel; not allowed with shared"),
  "shared?": type("boolean").describe("Visible to every account; root channels only"),
}).narrow(
  (p, ctx) => !(p.shared && p.parent !== undefined) || ctx.mustBe("a root channel when shared"),
);
```

- Delete `createChannelGroupParameters` and `commsCreateChannelGroup`, and drop the tool from the returned list. `comms_create_channel` maps `parent` to `parentAgentHandle` and `shared` to `shared`.

Tests: the schema refuses `parent` together with `shared: true`. `comms_create_channel` sends `parentAgentHandle`. The tool list has no `comms_create_channel_group`.

### T4. UI and client

Files: `apps/ui/src/{comms.ts,comms-stub.ts,store.ts}`, `apps/ui/src/live/{adapt,comms-state,stream,comms-fake}.ts`, `apps/ui/src/components/LeftSidebar.tsx`, `packages/compass-client/src/index.ts`, and their tests.

- Drop `ChannelGroup`, `adaptChannelGroup`, the `channelGroups` state, the `listChannelGroups` snapshot read, and the `channelGroupChanged` event case. `adaptChannel` maps `shared`.
- `channelSections` changes shape. `export interface ChannelSections { shared: Channel[]; root: Channel[] }` and `export function channelSections(channels: readonly Channel[]): ChannelSections`. DMs stay excluded. `shared` holds `c.shared` channels and `root` holds the rest. `LeftSidebar.tsx` calls it on `unclaimed()`, the member channels the tree does not claim, so non-member shared channels stay in browse (`browsableChannels`). The Shared section renders `shared`. The root band renders `root`, as the `group: undefined` section does today.

Tests: `comms-state.test.ts` and `stream.test.ts` drop the group cases. A member shared channel renders in the Shared section and under no agent. A non-member shared channel appears only in browse. `LeftSidebar.test.tsx` renames "owner-grouped channels render in the trailing root band" to describe root channels.

### T5. e2e fixture

Files: `go/e2e/fixture.go` and `go/e2e/legcomms_tenant_test.go`. Replace `createSharedGroup` with a `CreateChannel` call that sets `shared: true`, and rewrite the group-arm prose in `legcomms_tenant_test.go`. Keep each test's cross-owner visibility assertion.

### T6. Docs and citation sweep

- `docs/specs/product/compass.md`: rewrite the channel-group text as "agent tree plus shared root channels".
- Leave the prior record §1 and `docs/designs/agent/compass-manager-comms-substrate/design.md` ("always provisioned GROUPED") as history. The flipped rows point here.
- Code-comment sweep for the flipped ids: `grep -rn "DL-344\|DL-395\|DL-397\|DL-295" go apps packages proto` has no hits on main today. Re-run it after rebase. Remove the comments in `store/coordination.go` that cite "design.md:570-573" (the GROUPED home).
- `go/internal/store/presence_reads.go`: rewrite the "SHARED-grouped channels" comment.
- Done check: run `grep -rlE --exclude-dir=gen --exclude-dir=db --exclude-dir=node_modules --exclude=spine.test.ts "channel_groups|ChannelGroup|parent_group_id|group_id|GroupID|groupId|channels_group_|SHARED-group|SHARED group|grouped channel|__dm__|__coordination__|__linear__|create_channel_group|group_name|reserved DM group|reserved group" go apps packages proto docs/specs docs/concepts config`. It returns nothing when the work is done. On main it lists 73 git-tracked files (run it on a clean checkout), all group code or group prose. The excludes cover generated trees (`gen/`, `go/internal/store/db/`), installed packages, and the one false positive: `apps/ui/src/keyboard/spine.test.ts` uses `groupId` for keyboard roving groups. The pattern has no `GROUPED` or bare `grouped`, because `packages/compass-agent` ("GROUPED per topic") and `apps/ui/src/board.ts` ("grouped by `issue.assignee`") use them for other things.

## Tasks

- [ ] One PR (see *Landing*), one commit per task:
  - [ ] T1: schema, queries, store, store tests; sqlc regen.
  - [ ] T2: proto, comms, runnerhub, auth gate, fabric kind, their tests; buf regen.
  - [ ] T3: agent tools, AGENTS.md, the comms-playbook skill and `docs/concepts/tools.md` (`comms_create_channel` gains `parent`/`shared`; `comms_create_channel_group` deleted).
  - [ ] T4: UI and compass-client drop groups; a Shared section over `shared` channels.
  - [ ] T5: e2e fixture creates shared channels.
  - [ ] T6: product spec rewrite and citation sweep.
- [ ] Hold the merge until Matt rules on DL-487..490. Then fold the OQ answers into D5 and D2, cut the answered OQs, and rewrite the eight statuses to `(Matt, <date>)`.
- [ ] Dev DB recreate per RIG-4927 after the PR merges.

## Open Questions

### OQ1. Drop `channel_groups` entirely, or keep a flat reserved-namespace table?

- (a) Drop it. System homes become `channels.system_namespace` + `namespace_owner_id`. No RPC can write those columns, so the reserved-name guards go, and the homes get a real unique key. Cost: three upserts are rekeyed.
- (b) Keep a flat `channel_groups` (no `parent_group_id`, no visibility) for the three homes only. The diff is smaller. But a second container type stays, and so do the guards listed in D2 *Squats*.

**Recommendation:** (a). The Plan builds (a). Delta for (b): T1 keeps `channel_groups (id, name, owner_user_id, tenant_id)` with a unique `(owner_user_id, name)` key, keeps `channels.group_id` for system rows only, keeps `EnsureOwner*GroupTx` and `isReservedGroupTx` with `coordinationGroupName` added, and drops only nesting, visibility, and the lattice. DL-295 stays Active.

### OQ2. What replaces SHARED-group grants?

- (a) A `channels.shared` flag on root channels, with a unique name per tenant. It keeps DL-397's "never in a tree".
- (b) Remove SHARED spaces. Cross-owner reach uses membership and peering (DL-395) only. No server path mints a SHARED group today.
- (c) Keep SHARED groups as the only group use. The lattice and the group RPCs stay.

**Recommendation:** (a). It keeps a product affordance Matt ruled on (DL-397) for the cost of one column. Delta for (b): drop `shared`, `channels_shared_*`, and the predicate arm; T3 drops `shared?`; T4 drops the Shared section; T5 uses explicit cross-owner members. Delta for (c): this record shrinks to OQ1, and DL-397's CHECK clause stays in force.

### OQ3. How does the group wire go?

- (a) Remove without `reserved` (DL-186) and add `shared` at fresh numbers (`Channel.shared = 13`, `CreateChannelRequest.shared = 8`).
- (b) The same, plus `reserved` lines for each removed number and name, as `agent_gateway.proto` still does for `initial_prompt`.
- (c) Mark the surface `[deprecated = true]` and stub the RPCs as `Unimplemented`.

Reusing `Channel` 3 and `CreateChannelRequest` 2 as `bool` is not an option. It changes the wire type at a reused number, and the pinned agent image and stale UI tabs would misdecode it (D5).

**Recommendation:** (a). Delta for (b): T2 adds `reserved 3; reserved "group_id";` to `Channel`, the matching lines for 2, 7, `group_id` and `group_name` to `CreateChannelRequest`, and `reserved` lines for the removed oneof arms. Delta for (c): T2 keeps the messages and returns `CodeUnimplemented`; T3 and T4 still drop their callers.

### OQ4. Do removed oneof arms stay as permanent holes?

The removed arms are `SubscribeCommsResponse` 13 and `CommsCallRequest`/`CommsCallResult` 9/10. DL-186's one-time pass renumbered densely ("no holes"). Since then, one field-local comment in `agent_gateway.proto` (on `trigger_traceparent`) says "renamed never renumbered (OQ-1b)". Its arm numbers skip only numbers that message-level fields hold (`ForgeCallRequest` 12–13, `CommsCallRequest` 10). What is new here is a number no field uses at all.

- (a) Permanent holes, never renumbered. This extends the OQ-1b precedent to numbers no field uses.
- (b) Renumber later, at a planned break such as GA.

A renumber now is not an option. It breaks the pinned agent image and stale UI tabs. In `CommsCallRequest` it puts `open_dm` on 10, which is `trigger_traceparent`.

**Recommendation:** (a). Matt confirms the extension for both `agent_gateway.proto` and the public `comms.proto`. DL-490's hole clause waits on this answer. Delta for (b): none now; the Global Constraint "Never renumber" gains "until GA", and the GA plan owns the renumber.

### OQ5. Remove the RIG-2673 B arm?

Matt ruled B (2026-08-25) and directed follow-up A, per the org-mgmt record. This record removes B's `comms_create_channel_group`, as A implies. B's ledger row was never minted, so no row flips.

- (a) Remove the arm. `comms_create_channel` gains `parent?` and `shared?` (D4).
- (b) Keep a `comms_create_channel_group` name as an alias that creates a shared root channel.

**Recommendation:** (a). An alias keeps a tool name for a concept that no longer exists. Delta for (b): T3 keeps the tool with `name` and `shared` only, mapped to `CreateChannel`. Either way, the org-mgmt record's DL-new-5 was never minted. Whoever mints it should say "channels", not "channel groups".
