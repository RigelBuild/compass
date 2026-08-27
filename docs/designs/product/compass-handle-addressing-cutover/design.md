# Design: Contract-wide id→handle cutover for compass request fields (RIG-2751)

Status: Draft

Tracking: RIG-2751 (Matt ruled Option A, contract-wide, 2026-08-25; the storage
shape, owner-qualified wire, and rename/reclaim policy were negotiated to a
FINAL contract and Matt-ratified 2026-08-26 with "LGTM, can start" — the whole
contract is frozen; this record designs the how and freezes on merge)

## Problem / Intent

Matt ruled (RIG-2751, Option A expanded contract-wide): **every request-input
account field on the compass proto surface takes a `@handle`, and the server
resolves handle→`account_id`** — no agent and no client UI ever resolves an id.
An unresolvable handle is an in-band NOT_FOUND. Today the request contract is
id-typed (`member_account_ids`, `agent_account_id`, …), which forces the caller
to hold a directory it does not have: the agent's only account surface is the
scope-limited roster (`CommsCallRequest` carries no ListAccounts arm,
`proto/compass/v1/agent_gateway.proto:106-114`), so an agent literally cannot
resolve an arbitrary handle to an id client-side. This record designs the HOW
of the cutover — the whether is frozen.

## Approach

### The storage contract: `account_handles` + owner-qualified resolution

Handle→id resolution is not a flat global lookup. Matt ratified a two-namespace
contract (RIG-2751, 2026-08-26): **user and system handles are globally unique;
agent handles are unique only within their owner's namespace**, and an agent is
always addressed owner-qualified (`matt/compass-ux`). Two different humans may
each own an agent named `compass-ux`; the owner qualifier is what disambiguates
them, so resolution is `(owner, handle)` for agents and bare `handle` for
users/system.

Today the handle lives directly on `accounts` as a single global-unique column
(`handle TEXT NOT NULL UNIQUE`, `go/internal/store/migrations/0001_init.sql:38`),
which cannot express the per-owner agent namespace. The contract moves handle
ownership into a dedicated table:

- **`account_handles(account_id TEXT PK/FK→accounts(id), handle TEXT NOT NULL,
  owner_user_id TEXT NULL FK→user_accounts(account_id))`** — one row per account.
  `owner_user_id` is NULL for user and system accounts, the owning user's id for
  agent accounts (it mirrors `agent_accounts.owner_user_id`,
  `0001_init.sql:76`).
- **Two partial-unique indexes**, which double as the resolution index:
  - `UNIQUE(handle) WHERE owner_user_id IS NULL` — user/system handles are
    globally unique (preserving today's `accounts.handle` global-unique
    invariant for the human/system tier).
  - `UNIQUE(owner_user_id, handle) WHERE owner_user_id IS NOT NULL` — agent
    handles are unique per owner.
- An agent handle MAY overlap a global user handle with no collision at resolve
  time, because an agent is only ever looked up owner-qualified and a user only
  ever looked up bare — the two indexes never contend on the same lookup.

**Resolution at the edge** (the site §"Where resolution lives" names) parses the
owner qualifier first, then resolves in the matching namespace:

- `matt/compass-ux` → resolve `matt` bare in the user/system index → its
  `account_id` is the `owner_user_id` → resolve `compass-ux` in the agent index
  under that owner.
- A bare handle from an agent caller defaults to the **caller's own owner**
  namespace (`compass-ux` from one of matt's agents means `matt/compass-ux`),
  resolved from the session→account→`owner_user_id` the edge already holds.
- A bare user/system handle (`matt`, `compass`) resolves in the global index.

**Rename is in-place for both tiers**: `UPDATE account_handles SET handle = $new
WHERE account_id = $id`, no history, no tombstone, no `retired_at`, no
reservation table. The partial-unique index enforces the new handle is free at
rename time. **Reclaim is allowed for both tiers** — a freed handle (user or
agent) may be re-registered — so nothing needs remembering. A user rename has
**zero cascade to agents**: agents key on `owner_user_id` = the owner's stable
`account_id` (`0001_init.sql:76`), never the owner's handle, so a user rename
touches exactly one `account_handles` row and every owned agent re-renders
through the new owner handle automatically.

**Cross-human reclaim safety is NOT solved here.** Reclaim lets a freed user
handle be re-registered by a *different* human, so a stale `matt/compass-ux`
addresser could resolve into a stranger's namespace. That blast radius is closed
one layer up, at the authorization edge, not by reserving handles: cross-user
agent-to-agent reachability requires a bilateral owner-peering approval (filed
**RIG-2796**), so the new owner of a reclaimed handle receives nothing from
another fleet until the two owners have mutually approved. The handle is just an
address; authorization is the peering edge, not the name. RIG-2796 sits ABOVE
this contract and does not reopen the storage shape.

### The pattern being mirrored: `from_handle`

The contract already has one field where the server owns the handle↔id
boundary: the denormalized `from_handle` on Steer/Deliver controls
(`proto/compass/v1/agent.proto:194-199` and `:223-229` — "the Server resolves
it once when wrapping the AgentControl"). The resolution site is
`go/internal/delivery/consumer.go:398-413` (`authorHandle`), which calls the
store's `GetAccount` (`consumer.go:109-113`) and denormalizes the handle onto
the wire op (`deliverOp`/`steerOp`, `consumer.go:370-393`). That is the shape
this cutover generalizes, in the opposite direction: the **request** carries
the handle, and the server resolves handle→id at the service edge before any
store call.

### Where resolution lives: the service edge, not the store

Store signatures stay id-typed (`store.AccountID` everywhere:
`CreateChannel` `go/internal/store/channels.go:81`, `UpdateChannelMembers`
`channels.go:407`, `SetChannelPolicy` `channels.go:596`, `OpenAgentWorkspace`
`channels.go:818`, `ReparentAgent` `go/internal/store/accounts.go:448`). The
comms handler edge already owns wire→store mapping
(`accountIDsFromWire`/`memberUpdatesFromWire`,
`go/internal/comms/mapping.go:264,280`); resolution slots in exactly there —
the wire converters become handle resolvers that consult the store once per
request, then hand ids to the unchanged store layer. The lifecycle
(`DespawnPeer`) equivalent lives in `go/server/lifecycle.go` where
`req.GetAgentAccountId()` is read today (`lifecycle.go:221`).

Two store lookups back the resolvers, both re-pointed at `account_handles` (the
handle column moves off `accounts` per §"The storage contract"):

- **`AgentByHandle` resolves owner-qualified**: it takes the owner id (from the
  parsed `owner/` qualifier, or the caller's own `owner_user_id` for a bare
  handle) and the agent handle, and looks up the agent index
  (`UNIQUE(owner_user_id, handle) WHERE owner_user_id IS NOT NULL`). It stays
  non-elevating and agent-asserting; an unknown, non-agent, or wrong-owner
  handle → `ErrNotFound` with a message indistinguishable from unknown. Today's
  `AgentByHandle` (`go/internal/store/accounts.go:631-668`) resolves against the
  global `accounts.handle`; the cutover re-keys it to `(owner_user_id, handle)`
  over `account_handles`.
- A new general **`AccountsByHandles`** batch lookup for member/owner fields,
  which legitimately name users as well as agents. Each input is an
  owner-qualified or bare handle; the resolver parses the qualifier per input
  and resolves in the matching namespace (bare → user/system global index or
  the caller's own agent namespace; `owner/agent` → that owner's agent
  namespace). No general public by-handle lookup exists today —
  `adminByHandle`/`systemByHandle` are private and subtype-asserting
  (`accounts.go:107-111,178-183`).

### Error contract: in-band NOT_FOUND, oracle-safe

An unresolvable handle maps to `store.ErrNotFound` → `connect.CodeNotFound`
through the existing edge mapping (`edgeError`,
`go/internal/comms/context.go:47-68`), which the agent gateway renders in-band
as `CommsCallError`/`LifecycleCallError` rather than a transport teardown.

The not-found/forbidden merge is NOT a uniform existing invariant — only the
despawn path has it today (unknown, non-agent, and foreign-owner targets all
collapse into one indistinguishable `errPeerNotFound`,
`go/server/lifecycle.go:121-126,208-256`). The other flipped handlers must be
BROUGHT to that posture, because edge resolution turns their current splits
into a handle-enumeration oracle: an unknown handle gets the resolver's
NOT_FOUND while a real-but-foreign handle gets the handler's current distinct
error, and handles — unlike ids — are guessable. The invariant this record
mandates (DL-269): for every handle-addressed target, ANY post-resolution
authority/visibility failure returns the SAME
NOT_FOUND-naming-the-submitted-handle the resolver emits for an unknown
handle. Per-handler remap table:

| Handler | Today | Remap |
| --- | --- | --- |
| DespawnPeer | already merged as NotFound (`go/server/lifecycle.go:121-126`) | unchanged |
| CreateAgent parent | split: unknown → NotFound, foreign → `CodePermissionDenied` "parent agent %q has a different owner" (`go/internal/comms/comms.go:124-133`) | foreign → NotFound naming the submitted handle |
| ReparentAgent | merged, but as `ErrPermissionDenied` "caller may not re-parent agent %q" (`go/internal/store/accounts.go:507-509`) | → NotFound naming the submitted handle |
| OpenAgentWorkspace | invisible target → store NotFound naming the resolved ACCOUNT ID (`%w: agent %q`, `go/internal/store/channels.go:839-840`), and `edgeError` maps store errors VERBATIM (`go/internal/comms/context.go:47-68`) — the resolved id of an invisible account would leak in the message | re-key the message to name the submitted HANDLE, never the resolved id |

This deliberately changes two public error codes (ReparentAgent and the
CreateAgent parent check: PermissionDenied → NotFound). That is a contract
change made on purpose — mandated by DL-269's oracle invariant, not an
accident of the refactor.

**Message parity**: `AgentByHandle` already keeps the unknown-handle and
wrong-subtype messages identical (`accounts.go:659-665`); the new batch
resolver holds the same line for member fields. Note there is NO existing
member-field NOT_FOUND to inherit: today an unknown member account surfaces as
`ErrInvalidArgument` "unknown member account %q" via FK violation
(`go/internal/store/channels.go:157-158`, `upsertMemberErr`
`channels.go:569-573`) — the NOT_FOUND posture for member handles is DEFINED
by this record, not carried over. Whether member resolution is additionally
visibility-scoped (invisible ≡ unknown) is OQ-6, the one new load-bearing
fork; this record recommends scoped.

### GetRoster's dual vantage

`GetRosterRequest.agent_account_id` is dual-path today: an agent caller leaves
it empty and is session-resolved to itself; a human/UI caller may name a
vantage (`proto/compass/v1/comms.proto:698-704`, vantage defaulting in
`go/internal/comms/roster.go:24-36`; the agent tool deliberately never sets it,
`packages/compass-agent/src/comms.ts:729-733`; the UI stream also sends it
empty, `apps/ui/src/live/stream.ts:99-101`). The cutover keeps the exact
semantics with a handle-typed field: empty ⇒ caller vantage (agent
session-resolved, unchanged); non-empty ⇒ a handle the server resolves via
`AgentByHandle`. No ambiguity arises because the two paths were already
discriminated by emptiness, not by type. The field's final name is an Open
Question (below), not its mechanics.

Roster's ERROR posture is DEFINED here, not inherited — today a bogus or
invisible id vantage produces NO resolution error at all: the tree is built,
then clipped to the caller-visible set (`roster.go:38-53`), degrading to an
empty/clipped roster. After the flip, an unknown handle → NOT_FOUND while a
real-but-caller-invisible handle would resolve (`AgentByHandle` is global — no
viewer scoping, `accounts.go:638-668`) and return a clipped, likely empty,
roster SUCCESS — a NOT_FOUND-vs-empty-success vantage-probe oracle. Therefore:
post-resolve, the vantage handle MUST be in the caller-visible set (the same
`ListAccounts` projection the clip already fetches at `roster.go:47`), and an
invisible vantage maps to the identical NOT_FOUND an unknown handle gets. T3
carries the test leg.

### Explicit non-goal: responses, stored state, and events keep ids

**Response/stored/event account fields do NOT flip.** Handing back handles on
responses would push id→handle resolution onto the client — the opposite of
the ruling. For the UI lane this holds: the UI acts on ids the server already
gave it (roster entries, channel snapshots, message authors all carry ids
today and the UI joins them locally, e.g. `apps/ui/src/live/adapt.ts:173-208`);
the concern the ruling fixes is INPUT resolution only. The AGENT lane is the
known exception: the agent's ListMessages tool renders raw author ids into the
model-visible fence (`author="${attr(m.authorAccountId, fence)}"`,
`packages/compass-agent/src/comms.ts:694`) and the agent has no id→handle
resolver (`CommsCallRequest` carries no ListAccounts arm,
`proto/compass/v1/agent_gateway.proto:106-114`) — a concrete surface carried
in OQ-5, fixable by DL-270's own additive-sibling mechanism without
reopening this non-goal. Unchanged, deliberately:
`Channel.{member,subscriber}_account_ids` / `owner_account_id`
(`comms.proto:233,238,243`), `AgentWorkspace.agent_account_id`
(`comms.proto:293`), `Topic.created_by_account_id` (`comms.proto:312`),
`ChannelChanged.removed_account_ids` (`comms.proto:526`),
`AgentPresenceChanged.agent_account_id` (`comms.proto:555`),
`RosterEntry.agent_account_id` (`comms.proto:723` — it already carries
`handle` at `:724` beside the id, the right dual shape),
`SpawnPeerResponse.agent_account_id` (`agent_gateway.proto:175`). If a UI
surface genuinely needs a handle a response lacks, that is an Open Question to
surface, not a reflex flip (OQ-5).

### Sequencing against the in-flight org-management stack

PRs #628 (proto arms, draft) and #630 (handlers, draft, stacked on #628) are
held in draft specifically so their three new `CommsCallRequest` arms
(`create_channel` = 7, `update_members` = 8, `create_channel_group` = 9) ship
handle-first. Because #628 reuses the `comms.proto` payload messages verbatim,
flipping the payload messages here flips the arms automatically. The order:

1. **This cutover's proto change lands first** (T1): `comms.proto` +
   `agent_gateway.proto` request fields flip, all four gen lanes regenerate.
2. **#628 rebases onto it** (regen-only delta — its arms reference the
   now-handle-typed payloads verbatim).
3. **#630's handlers rebase** to do the server-side resolution for the new
   arms (its adapters call the T3 resolvers).
4. **compass-agent #632 rewires** its tool params to handles
   (`member_handles` etc. instead of `member_account_ids`).
5. **compass-runner / UI consumers** last (the UI sends none of the flipped
   fields non-empty today — `stream.ts:99-101` — so its rewire is
   forward-looking, not a breakage fix).

**Skew window (bounded, not denied)**: live agent containers outlive a server
redeploy, so an agent spawned pre-cutover holds the old generated bundle and
old tool schemas (`despawnParameters` still takes `agent_account_id`,
`packages/compass-agent/src/lifecycle.ts:91-95`) and sends an id string into
the now-handle-semantic field (same field number, same string wire type). The
server resolves that id AS a handle → miss → in-band NOT_FOUND tool error
until the container is respawned on the new bundle. Acceptable because the
failure is in-band (no decode error, no transport teardown — a point FOR
rename-in-place), the affected surface for existing agents is despawn +
roster-vantage only, and the fleet respawns on deploy cadence. If fleet
respawn is NOT guaranteed on the deploy that ships the cutover, T5 adds the
explicit respawn step and names who forces it.

## Field inventory (verified against current main)

Request-input account fields that flip id→handle. Line numbers are from the
current tree at authoring time.

| # | Message.field | Location | Handler read site |
| --- | --- | --- | --- |
| 1 | `CreateChannelRequest.member_account_ids` (field 4) | `proto/compass/v1/comms.proto:647` | `go/internal/comms/comms.go:228` (`accountIDsFromWire`) |
| 2 | `UpdateChannelMembersRequest.add_member_account_ids` (2) | `comms.proto:658` | `comms.go:250` via `memberUpdatesFromWire` (`mapping.go:280`) |
| 3 | `UpdateChannelMembersRequest.remove_member_account_ids` (3) | `comms.proto:660` | same |
| 4 | `UpdateChannelMembersRequest.subscribe_account_ids` (4) | `comms.proto:662` | same |
| 5 | `UpdateChannelMembersRequest.unsubscribe_account_ids` (5) | `comms.proto:664` | same |
| 6 | `ReparentAgentRequest.agent_account_id` (1) | `comms.proto:673` | `comms.go:277` |
| 7 | `ReparentAgentRequest.new_parent_agent_id` (2) | `comms.proto:675` | `comms.go:278` |
| 8 | `SetChannelPolicyRequest.owner_account_id` (3) | `comms.proto:688` | `comms.go:476` |
| 9 | `GetRosterRequest.agent_account_id` (2) — dual-path, see Approach | `comms.proto:703` | `go/internal/comms/roster.go:27-36` |
| 10 | `OpenAgentWorkspaceRequest.agent_account_id` (1) | `comms.proto:758` | `comms.go:300` |
| 11 | `CreateAgentRequest.parent_agent_id` (3) | `comms.proto:590` | `comms.go:124-141` |
| 12 | `DespawnPeerRequest.agent_account_id` (1) | `proto/compass/v1/agent_gateway.proto:184` | `go/server/lifecycle.go:221` |
| 13 | The 3 new `CommsCallRequest` arms on PR #628 (`create_channel` = 7, `update_members` = 8, `create_channel_group` = 9) | #628 head, `agent_gateway.proto` | reuse rows 1–5 verbatim (`create_channel_group` carries no account field — `comms.proto:607-613` — it rides the flip only via its sibling arms' payloads) |

Rows 7 and 11 were not in the original RIG-2751 enumeration but are the same
class (an agent-account input a UI would otherwise have to resolve): flipping
`ReparentAgentRequest.agent_account_id` while leaving `new_parent_agent_id`
id-typed would leave the caller resolving an id for the same request.

**The precedent to mirror**: `SpawnPeerRequest.handle`
(`agent_gateway.proto:168`) — the spawn tool already takes a handle and the
server owns the id (`SpawnPeerResponse.agent_account_id`,
`agent_gateway.proto:175`).

**Out of scope**: `channel_id` / `group_id` / `parent_group_id` (not account
handles); every response/stored/event field (see the non-goal above); the
`asker`/author fields on stored messages (server-derived, never
client-supplied). The compass.proto admin lane is OQ-4.

## Global Constraints

- **Proto conventions**: buf lint runs as `compass-proto:lint`; the envelope
  naming exceptions are documented in `agent_gateway.proto:22-28`. Comment
  every flipped field with its resolution semantics (mirror the
  `from_handle` comments, `agent.proto:194-199`).
- **Pre-GA breaking allowance**: the buf breaking gate is removed pre-dogfood
  (`proto/moon.yml:169` — "RE-ADD AT GA", SEA-1922/SEA-1951; RIG-2675). A
  breaking rename is allowed and this record uses it; DL-186
  (`docs/designs/DECISIONS.md:199`, Active) rules rename-in-place keeping
  field numbers — renumber+reserve would re-add `reserved` markers DL-186
  stripped (OQ-1b is a confirm of this, not an open fork).
- **Codegen**: after any schema edit run `moon run compass-proto:gen` from the
  repo root (regenerates all four lanes: `packages/compass-client/src/gen`,
  `go/gen`, `packages/compass-agent/src/gen`, `go/internal/gen` —
  `proto/moon.yml:33-68`); CI gates on `compass-proto:drift` and
  `compass-proto:gen-fence` (`proto/moon.yml:69-171`).
- **Resolution pattern**: handle→id resolution lives at the SERVICE EDGE
  (comms handler / lifecycle handler); `store.*` signatures stay id-typed.
  Mirrors `from_handle`'s server-side-resolution posture
  (`go/internal/delivery/consumer.go:398-413`).
- **Error posture**: unresolvable handle → `store.ErrNotFound` →
  `connect.CodeNotFound` via `edgeError` (`go/internal/comms/context.go:47-68`),
  in-band on the gateway. The oracle invariant is inviolable: a foreign,
  non-visible, or wrong-subtype handle is byte-identical to an unknown one —
  code AND message, and no error message may name a resolved account id. Today
  only the despawn path has the merge (`go/server/lifecycle.go:121-126`); the
  other handlers are brought to it via the §Error contract remap table.
  Message parity per `accounts.go:659-665`.
- **Red-green**: every task lands tests first (rule://red-green-testing);
  pgtests for store/handler tasks, bun:test for TS tasks.

## Plan

### T0 — schema: `account_handles` migration + backfill

A new migration `go/internal/store/migrations/0002_account_handles.sql` (the
first after `0001_init.sql`; the migrations dir holds exactly that one file
today) creates `account_handles(account_id TEXT PK/FK→accounts(id) ON DELETE
RESTRICT, handle TEXT NOT NULL, owner_user_id TEXT NULL FK→user_accounts(account_id)
ON DELETE RESTRICT)` with the two partial-unique indexes from §"The storage
contract" (`UNIQUE(handle) WHERE owner_user_id IS NULL`; `UNIQUE(owner_user_id,
handle) WHERE owner_user_id IS NOT NULL`). It backfills one row per existing
account — `owner_user_id` = the agent's `owner_user_id` for agents
(`agent_accounts.owner_user_id`, `0001_init.sql:76`), NULL for users/system —
then drops the now-redundant global-unique constraint on `accounts.handle`
(`0001_init.sql:38`). The `accounts.handle` COLUMN stays (display reads and the
`0001_init.sql` seed still populate it; it is no longer the resolution key, and
dropping the column is a separate cleanup out of scope here); the resolution
source of truth becomes `account_handles`.

This task is INDEPENDENT of the proto flip (T1) — schema vs wire — and lands in
the same server PR. The store resolvers (T2) and every edge resolver (T3/T4)
read `account_handles`, so T0 lands before or with T2 in the stack.

- **Interfaces**: the migration file + the store's handle read/write paths
  re-pointed at `account_handles` (`CreateUser`/`CreateAgent` insert a handle
  row in the same tx that inserts the account; a rename UPDATEs the handle row).
- **Test cycle**: pgtests — backfill parity (every pre-migration account has
  exactly one handle row with the right `owner_user_id`), the two uniqueness
  invariants (a second global `matt` rejected; a second `matt/compass-ux`
  rejected; `matt/compass-ux` and `alice/compass-ux` coexist; an agent handle
  overlapping a user handle coexists), rename in-place both tiers, reclaim of a
  freed handle both tiers.

### T1 — proto flip + regen (lands first; #628/#630 rebase onto it)

Rename and re-comment every row of the inventory in `comms.proto` and
`agent_gateway.proto` to its handle form (working names, pending OQ-1:
`member_handles`, `add_member_handles`, `remove_member_handles`,
`subscribe_handles`, `unsubscribe_handles`, `agent_handle`,
`new_parent_handle`, `owner_handle`, `parent_handle`). Semantics comment on
each: "a `@handle`; the server resolves it to an account id; unknown →
NOT_FOUND". Regenerate all four lanes.

- **Interfaces**: proto fields per the inventory table (same string wire type;
  field numbers kept in place under the OQ-1b working assumption).
  `GetRosterRequest.agent_handle`: empty ⇒ caller vantage (unchanged
  session-resolved semantics), non-empty ⇒ server-resolved agent handle.
- **Test cycle**: `moon run compass-proto:lint compass-proto:drift
  compass-proto:gen-fence`; Go/TS builds red until T3/T6 land in the same
  stack — T1 therefore lands as the first commit of the server PR, not as a
  green standalone merge to main.

### T2 — store batch handle resolver

Add a batch resolver in `go/internal/store/accounts.go` over `account_handles`
(T0's table), plus a small edge helper that parses the `owner/handle` qualifier
before it. Each input is owner-qualified or bare; the resolver splits on the
first `/`, resolves the owner segment in the user/system index
(`UNIQUE(handle) WHERE owner_user_id IS NULL`), then the agent segment in that
owner's agent index (`UNIQUE(owner_user_id, handle)`); a bare handle resolves
either as a user/system handle in the global index or as an agent handle in the
**caller's own** owner namespace (the edge supplies `callerOwner` from
session→account). Its signature also depends on OQ-6 (member-resolution
visibility scoping); recommended form
`Store.AccountsByHandles(ctx context.Context, viewer AccountID, callerOwner AccountID, handles []QualifiedHandle) (map[string]AccountID, error)`,
one query per namespace against `account_handles` intersected with
`accountVisibleFromWhere` (`accounts.go:670-683`) so an invisible handle misses
exactly like an unknown one; if Matt rules unscoped (OQ-6 option b) the `viewer`
param drops. No subtype assertion beyond the index split (member/owner fields
legitimately name users and agents; never the system account — exclude
`system_accounts` rows, matching the roster/delivery exclusion in
`go/internal/store/system_account_exclusion_pgtest_test.go:11-14`). Any missing
handle → `ErrNotFound`; the resolver returns the full hit map, so the
set-difference is free and the error names ALL unresolved handles (in their
submitted `owner/handle` spelling), not just the first, with the same message
template as `AgentByHandle` (`accounts.go:655,665`). Agent-typed singular fields
reuse `AgentByHandle`, re-keyed to `(owner_user_id, handle)` over
`account_handles` per §"Where resolution lives" (`accounts.go:638`).

- **Interfaces**: `AccountsByHandles(ctx context.Context, viewer AccountID,
  callerOwner AccountID, handles []QualifiedHandle) (map[string]AccountID, error)`
  over `account_handles` — the `viewer` param is RULING-DEPENDENT on OQ-6;
  `callerOwner` supplies the bare-agent-handle default namespace; a
  `QualifiedHandle` carries the parsed `{owner, handle}` (owner empty = bare).
  Atomic — any missing handle fails the whole call, the error naming every
  unresolved handle in its submitted spelling (per the OQ-2 working assumption).
- **Test cycle**: pgtests — round-trip, owner-qualified agent resolution
  (`matt/compass-ux` vs `alice/compass-ux` disambiguated by owner), bare agent
  handle defaulting to the caller's owner, bare user/system handle in the global
  index, missing-handle NOT_FOUND naming ALL missing handles, system-handle
  NOT_FOUND, invisible-handle ≡ unknown (OQ-6a leg), user+agent mixed
  resolution, empty-input no-op.

### T3 — comms edge resolution

Replace the id pass-throughs in `go/internal/comms/mapping.go:264,280` and the
handler read sites (`comms.go:228,250,277-278,300`, `comms.go:476`,
`comms.go:124-141`, `roster.go:27-36`) with resolvers: repeated member fields
resolve through
`AccountsByHandles`; singular agent fields (`agent_handle`,
`new_parent_handle`, roster vantage, workspace target, `parent_handle` on
CreateAgent) through `AgentByHandle`; `owner_handle` through
`AccountsByHandles` (an owner may be a user). Resolver misses flow through
`edgeError`; post-resolution authority/visibility failures on handle-addressed
targets are remapped per the §Error contract table (CreateAgent parent
foreign → NotFound; ReparentAgent's `ErrPermissionDenied` merge → NotFound;
OpenAgentWorkspace's store NotFound re-keyed to name the submitted handle,
never the resolved id). `memberUpdatesFromWire` keeps its merge-by-account
semantics, now keyed post-resolution so two spellings of one handle cannot
yield two conflicting MemberUpdates.

- **Interfaces**: comms handler signatures unchanged; new unexported
  `resolveHandles(ctx, st, []string) ([]store.AccountID, error)` +
  `resolveAgentHandle(ctx, st, string) (store.AccountID, error)` helpers in
  `go/internal/comms`; store calls unchanged
  (`channels.go:81,407,596,818`, `accounts.go:448`).
- **Test cycle**: pgtests per RPC — happy path, unknown-handle NOT_FOUND,
  D9-invisible-account parity (invisible ≡ unknown), the remap legs (foreign
  parent on CreateAgent, foreign target on ReparentAgent, invisible target on
  OpenAgentWorkspace — each byte-identical to unknown, code AND message),
  roster empty-vantage session-resolution regression AND roster
  invisible-vantage → NOT_FOUND (`roster_pgtest_test.go` extends).

### T4 — lifecycle despawn resolution

`go/server/lifecycle.go` Despawn: the ordering MUST preserve the documented
constant-query-shape bar (`lifecycle.go:225-241`: caller-first ordering makes
the unknown-target and foreign-target outcomes both run exactly two queries so
latency cannot distinguish them; a naive resolve-target-first would make
unknown = 1 query and foreign = 3 — the exact existence probe the merge exists
to prevent, on the MORE enumerable input). Order: resolve `callerOwner` via
`AgentOwner` FIRST (unchanged), THEN `AgentByHandle(handle)` — a miss folds
into the existing `errPeerNotFound` merge (`lifecycle.go:121-126,247-256`); a
hit compares `acc.OwnerUserID` against `callerOwner` directly (`AgentByHandle`
already selects `ag.owner_user_id` and returns the full `Account`,
`accounts.go:641-651`, so the separate `AgentOwner(target)` query is DELETED).
Both outcomes run exactly two queries; unknown handle ≡ foreign peer ≡
non-agent, byte-identical. Self-despawn guard (`lifecycle.go:221-224`)
compares post-resolution ids.

The despawn `agent_handle` is owner-qualified like every other agent handle; a
bare handle defaults to the caller's own owner namespace, which is exactly the
peer set despawn already scopes to (the `callerOwner` comparison), so the
common same-owner despawn stays a bare handle and the resolution re-keys to
`(callerOwner, handle)` over `account_handles` with no change to the
constant-query-shape ordering above.

- **Interfaces**: `DespawnAsAccount(ctx, caller store.AccountID, req
  *compassv1internal.DespawnPeerRequest)` unchanged externally; internal target
  derivation becomes handle-resolved.
- **Test cycle**: extend `lifecycle_pgtest_test.go` /
  `lifecycle_e2e_pgtest_test.go` — unknown-handle vs foreign-handle
  indistinguishability (code AND message, mirroring
  `lifecycle_e2e_pgtest_test.go:348-433`), self-despawn by own handle,
  idempotent re-despawn by handle.

### T5 — rebase the org-management stack (#628, #630)

PR #628 rebases onto T1 (regen-only: its arms reuse the now-handle-typed
payload messages verbatim). #630's adapters
(`CreateChannelAsAccount`/`UpdateChannelMembersAsAccount`, per its PR body)
inherit T3's resolution for free since they delegate to the shared handler
path; its pgtests gain unknown-handle legs.

T5 also owns the §Sequencing skew-window close-out: confirm live agents are
respawned on the deploy that ships the cutover; if fleet respawn is not
automatic on server deploy, this task adds the explicit respawn step and names
its operator.

- **Interfaces**: no new ones — a coordination task with its own verify
  (stack CI green post-rebase).
- **Test cycle**: #630's existing suites + new unknown-handle legs.

### T6 — compass-agent tool rewire

`packages/compass-agent/src/lifecycle.ts:91-95,181-217`: `agents_despawn_peer`
takes `agent_handle` (non-blank), builds `DespawnPeerRequest{agentHandle}`;
description drops "by its agent account id". `comms.ts` roster tool unchanged
(it never set the vantage, `comms.ts:729-733`). #632's three new tools
re-author their params handle-first (`member_handles` etc.) on its rebase.

- **Interfaces**: tool param schemas (`despawnParameters`,
  `lifecycle.ts:91-95`; #632's `create_channel`/`update_members` params);
  wire messages from the T1 regen.
- **Test cycle**: `lifecycle.test.ts` / `comms.test.ts` wire-shape asserts
  updated red→green; `bun test` in `packages/compass-agent`.

### T7 — UI/client sweep + smoke

The UI sends none of the flipped fields non-empty today
(`apps/ui/src/live/stream.ts:99-101` leaves the roster vantage empty; no UI
call site constructs Create/Update/Reparent/SetPolicy/OpenWorkspace requests
with account inputs — the id usages in `apps/ui/src` are all RESPONSE-side
joins, e.g. `adapt.ts:173-208`). Sweep confirms zero live request-side callers,
updates `comms-stub.ts` commentary if field names leak into docs, and runs the
UI suite against the regenerated client.

- **Interfaces**: none new; regenerated `packages/compass-client/src/gen`.
- **Test cycle**: `bun test` in `apps/ui`; grep-verify no `_account_ids`
  request construction remains outside response adapters.

## Tasks

- [ ] T0 — `0002_account_handles.sql` migration (table + two partial-unique
  indexes) + backfill + relax `accounts.handle` global-unique; store handle
  read/write re-pointed at `account_handles` + pgtests
- [ ] T1 — proto flip (inventory rows 1–12) + 4-lane regen; lint/drift/fence
  green
- [ ] T2 — `Store.AccountsByHandles` owner-qualified batch resolver +
  `AgentByHandle` re-keyed to `(owner, handle)` + pgtests
- [ ] T3 — comms edge resolution (mapping.go + handler sites) + pgtests
- [ ] T4 — lifecycle despawn handle resolution, merged NOT_FOUND + pgtests
- [ ] T5 — rebase #628/#630 handle-first; stack CI green
- [ ] T6 — compass-agent despawn tool + #632 tools rewired to handles + bun
  tests
- [ ] T7 — UI/client sweep, regenerated client, UI suite green

## Ledger impact

No existing DL row mandates id-typed request addressing
(`docs/designs/DECISIONS.md` grepped for handle/addressing: DL-094 covers
display attribution — "the bare `@handle`... owner resolved server-side" —
DL-188/DL-191 cover reserved system handles, DL-202 covers forge provider
addressing; none constrains request account fields). One DL row DOES bear on
the mechanics: **DL-186** (`DECISIONS.md:199`, Active) strips pre-dogfood
proto wire-compat — all `reserved` markers removed across compass/v1, live
fields densely renumbered, the buf breaking gate removed (re-armed at GA) —
which rules OQ-1b's renumber+reserve alternative OUT unless Matt overrides an
Active row. The cutover ADDS three rows on merge (next-free at authoring;
confirm at freeze):

- **DL-269**: every request-input account field on the compass proto
  contract is handle-typed; the server resolves handle→account_id at the
  service edge (the `from_handle` posture generalized); an unresolvable,
  invisible, foreign, or wrong-subtype handle is one indistinguishable in-band
  NOT_FOUND. No agent or client UI ever resolves an id.
- **DL-270**: response, stored, and event account fields stay id-typed
  (ids are the stable join keys clients already hold); a response that needs a
  handle for display carries it as an explicit sibling field (the
  `RosterEntry.agent_account_id`+`handle` dual, `comms.proto:723-724`), never
  by retyping the id field.
- **DL-271**: handle→id resolution is owner-namespaced, stored in a dedicated
  `account_handles(account_id, handle, owner_user_id NULL)` table with two
  partial-unique indexes (user/system handles globally unique; agent handles
  unique per owner). Agents are addressed owner-qualified (`matt/compass-ux`,
  bare = caller's own owner); users/system bare. Rename is in-place for both
  tiers; reclaim is allowed for both tiers (no history, tombstone, or
  reservation). Cross-human reclaim safety is the owner-peering authorization
  edge (RIG-2796), not handle reservation. The handle column stays on
  `accounts` for display only; `account_handles` is the resolution key.

## Open Questions

Matt ratified the storage/format contract on 2026-08-26 ("LGTM, can start"),
which CLOSED the forks that were open during negotiation: storage shape
(`account_handles` + two partial-unique indexes), the owner-qualified wire
(agents `owner/handle`, users/system bare), and rename/reclaim policy (in-place
both tiers, reclaim allowed, no history/reservation, cross-human safety →
RIG-2796). Those are now the frozen contract above, not open questions. The
questions below are the remaining wire-cutover forks the ratification did NOT
decide — mostly proto-naming taste plus two real scope forks (OQ-4 admin lane,
OQ-6 member visibility) — plus one new fork the owner qualifier introduces
(OQ-7, the qualifier grammar).

1. **Field naming (OQ-1)** — working assumption: `*_account_ids`→`*_handles`,
   `agent_account_id`→`agent_handle`, `new_parent_agent_id`→`new_parent_handle`,
   `owner_account_id`→`owner_handle`, `parent_agent_id`→`parent_handle`. Pure
   taste. Matt picks the final names.
2. **Field numbering (OQ-1b) — a confirm, not an open fork** — DL-186
   (`docs/designs/DECISIONS.md:199`, Active) already rules rename IN PLACE
   keeping field numbers: the renumber+reserve alternative would ADD
   `reserved` markers, contradicting an Active DL (same string wire type;
   pre-GA, single-repo, the breaking gate is off — `proto/moon.yml:169`).
   Caveat checked: DL-187's later `reserved 3` on `SpawnPeerRequest`
   (`agent_gateway.proto:170-171`) guards the semantic revival of
   `initial_prompt`, not wire compat — it does not reopen this.
   Rename-in-place is also what makes the §Sequencing skew window degrade
   gracefully (an old bundle's id string parses fine and fails resolution
   in-band, never a decode error). Matt confirms rename-in-place, or
   explicitly overrides DL-186.
3. **Repeated-field NOT_FOUND semantics (OQ-2) — a confirm** — working
   assumption ATOMIC: one bad handle in `add_member_handles` fails the whole
   request (matches the store's all-or-nothing transaction posture —
   `UpdateChannelMembers` runs one tx, `channels.go:407-410`), with the error
   naming ALL unresolved handles, not just the first — the batch
   `WHERE handle = ANY($1)` returns the full hit map, so the complete missing
   set is a free set-difference (a 50-member CreateChannel with 3 typos fails
   once naming all 3, not across 3 round trips). Alternative: partial
   success plus a per-handle error list — needs a response-shape change and
   weakens the idempotent-retry story; dismissed. Matt confirms atomic.
4. **GetRoster vantage field name (OQ-3)** — the dual-path mechanics are
   settled (empty ⇒ session-resolved caller; non-empty ⇒ UI-named handle), but
   the name `agent_handle` on a field an agent must always leave empty invites
   misuse. Alternative: `vantage_handle` — the better fit given the vantage
   posture §GetRoster now defines. Matt picks the name.
5. **compass.proto admin lane (OQ-4)** — `SpawnAgentRequest.agent_account_id`
   (`compass.proto:650`), `ProvisionAgentWorkspaceRequest.agent_account_id`
   (`compass.proto:567`), and `IssueTokenRequest.account_id`
   (`compass.proto:702`) are also request-input account fields, but on the
   admin/ops lane (adminOnly door; DL-253 dropped the spawn UI). Does the
   contract-wide ruling extend to them in this cutover, or do admin/ops
   callers (which receive ids from prior admin responses) keep ids?
6. **Response handles (OQ-5)** — no response field flips (the non-goal
   above), and the UI lane needs nothing today (it joins via its account
   directory, `apps/ui/src/comms.ts:139-151`). But one concrete surface IS
   identified: the agent's ListMessages tool renders raw author ids into the
   model-visible fence (`author="${attr(m.authorAccountId, fence)}"`,
   `packages/compass-agent/src/comms.ts:694`) and the agent has no id→handle
   resolver — the same cannot-resolve argument that motivated this cutover,
   pointed at a response. The fix is DL-270's OWN additive-sibling
   mechanism (an additive `author_handle` on `Message`, or a
   `from_handle`-style denorm on the agent list result), never a retype of
   the id field. Matt rules: ship the sibling with this cutover, or defer?
7. **Member-resolution visibility scoping (OQ-6, load-bearing)** — T2's
   batch resolver must pick a side the record previously assumed both of:
   its parity sentence promises invisible ≡ unknown, but an unscoped resolver
   cannot distinguish visible from invisible — every real handle resolves,
   making member-add a global handle-existence oracle AND letting a caller
   ATTACH an account outside its D9-visible set to its channel by guessing the
   handle (today's by-id path is FK-only, `channels.go:151-161,529-573`, with
   no visibility gate either — but ids are unguessable, so it never mattered).
   The owner qualifier does NOT close this — it disambiguates namespaces, not
   visibility (`alice/compass-ux` is well-formed whether or not the caller can
   see it). The options: (a) visibility-scoped — the T2 resolver intersects
   `accountVisibleFromWhere` (`go/internal/store/accounts.go:670-683`) after
   the namespace split; invisible ≡ unknown holds, and naming-a-member GAINS a
   visibility gate the id path never had (a semantic TIGHTENING: who may be
   named as a channel member). (b) unscoped — every real handle resolves,
   preserving today's FK-only permissiveness at the cost of an enumeration
   oracle under guessable handles. A real user-facing policy choice: can I add
   a teammate's agent I can't see to my channel? **Recommended: (a)** — the
   only option consistent with the ruling's oracle posture and DL-269. The T2
   interface signature depends on this ruling.
8. **Owner-qualifier grammar (OQ-7, NEW — introduced by the storage
   contract)** — the owner-qualified form `matt/compass-ux` needs a defined
   grammar before T1 comments the fields. Working assumption: `/` is the
   separator, the owner segment is a bare user/system handle (never itself
   qualified — no nesting), a leading `/` is illegal, exactly one `/` is
   permitted, and both segments obey the existing handle charset (the same
   grammar constraint the ownership layer already names for `owner=<handle>`,
   `docs/designs/product/compass-server-ownership-layer/design.md:559-563`). A
   bare handle (no `/`) is a user/system handle OR the caller's own agent —
   disambiguated by which index resolves it, users/system first. Open sub-fork:
   if a bare handle matches BOTH a user handle and one of the caller's own
   agents, which wins? Working assumption: user/system global index takes
   precedence (a human is never shadowed by one of your agents), so address
   your own agent that collides with a username by qualifying it
   (`matt/compass-ux`). Matt confirms the separator + the collision precedence,
   or picks another grammar.
