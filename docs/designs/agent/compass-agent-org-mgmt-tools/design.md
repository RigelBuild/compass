# Agent org-management tools (RIG-2673)

Status: Draft

Refs: RIG-2673 (parent RIG-1681). Design only — this record is the contract; no Go/proto/TS changes ship with it.

## Problem / Intent

Agents today have 5 comms tools and 2 lifecycle tools, but a Manager agent cannot CREATE org structure — channels, channel subscriptions, or role-bearing sub-Manager agents — so wave parity is unreachable from inside the mesh. The server RPCs already exist on `CommsService` (operator/UI-facing); the gap is purely that they are not exposed on the agent gateway. This record specifies the agent-gateway exposure and is the **cross-lane proto contract** that compass-agent (transport + tools + prompt) and compass-runner (gateway relay) build against.

## Approach

Expose org-management as **new arms on the existing agent-gateway call envelopes**, reusing the existing `CommsService` request/response messages verbatim as the arm payloads — exactly how `post` reuses `PostMessageRequest` today — executed server-side under the D9 server-resolved caller. Four ops, four shapes:

1. **`create_channel`** — new `CommsCallRequest` oneof arm carrying the existing `CreateChannelRequest`.
2. **`update_members`** — new `CommsCallRequest` oneof arm carrying the existing `UpdateChannelMembersRequest` (one arm covers join, subscribe-toggle, unsubscribe, and removal — the RPC is already the union: "One RPC covers join, subscribe-toggle, DM-expansion, and share-replacement", `proto/compass/v1/comms.proto:65` area, quoted below).
3. **`create_channel_group`** — new `CommsCallRequest` oneof arm carrying the existing `CreateChannelGroupRequest`.
4. **create-Manager** — NOT a new arm. A **required** `role` field plus a **required** `persona` field (both ruled required by Matt — see OQ-2) added to the **existing** `SpawnPeerRequest` on the `LifecycleCallRequest.spawn` arm, with the server dropping its `Persona: ""` / `Role: ""` hardcodes.

**`create_channel_group` is agent-exposed (Matt ruled B, 2026-08-25).** An earlier draft deferred group creation to operator-only; Matt's no-human-clicks principle voids that — everything (channels, groups, sub-managers, subscriptions) must be standupable by agents, humans reserved to the security boundary. So `create_channel_group` is a first-class arm here, reusing `CreateChannelGroupRequest` verbatim; the store's `requireGroupCreateAuthz` (`store/authz.go:87-113`) already D9-bounds it (own the parent, be an agent of the owner, or parent is SHARED). Matt also directed a **follow-up (A): collapse the channel-group tree into the agent tree** so the two parallel hierarchies (`channel_groups.parent_group_id` and `agent_accounts.parent_agent_id`, which already mirror per `migrations/0001_init.sql:58`) become one — that is its own design + migration (reshapes DL-135/DL-190), filed separately and explicitly NOT folded here.

### Why each op takes this shape

**Trust model (D9, confirmed).** Every CommsService RPC is authorized server-side against the caller resolved from the connection, never a request field:

> `proto/compass/v1/comms.proto:29-33`: "the caller is the account authenticated on the connection … never a field in a request, which would be spoofable. Every RPC is authorized server-side against that caller's visibility (D9's owner-gated access) … No message below carries a caller identity, for that reason."

The relay leg preserves this: the Runner is a pure forwarder and the hub resolves the session binding —

> `go/internal/runnerhub/relay_comms.go:7-15`: "The Runner is a pure forwarder: it sends RelayCommsCall{session_id, call} and asserts NO account. The SERVER resolves session_id -> agent account from THIS hub's own binding … and executes the call under that account via the CommsCaller … a session_id on the wire selects an account, it never carries one."

So an agent creating a channel or spawning a peer acts under its server-resolved account (and, for spawn ownership, its owner) with the identical authz, idempotency, and event fan-out a human caller gets:

> `go/internal/runnerhub/hub.go:218-221`: "The comms package implements it over the same PostMessage / ListMessages handler paths a human caller takes, so authz (D9), idempotency, and event fan-out are identical".

**`create_channel` / `update_members` reuse existing handlers wholesale.** Both RPCs and handlers exist:

> `proto/compass/v1/comms.proto` (service block): "Create a channel within a group — caller-authorized against the parent group. Emits ChannelChanged. `rpc CreateChannel(CreateChannelRequest) returns (CreateChannelResponse);`" and "Add or remove channel members and flip a member's subscribe opt-in — caller-authorized against channel visibility. Emits ChannelChanged. One RPC covers join, subscribe-toggle, DM-expansion, and share-replacement. `rpc UpdateChannelMembers(UpdateChannelMembersRequest) returns (UpdateChannelMembersResponse);`"
>
> `go/internal/comms/comms.go:239-254` (`CreateChannel`): `ch, err := c.store.CreateChannel(ctx, c.actorFromContext(ctx), store.NewChannel{...}); ... c.publishChannelChanged(ch, nil)`
>
> `go/internal/comms/comms.go:261-276` (`UpdateChannelMembers`): `ch, removed, err := c.store.UpdateChannelMembers(ctx, c.actorFromContext(ctx), store.ChannelID(req.Msg.GetChannelId()), memberUpdatesFromWire(req.Msg)); ... c.publishChannelChanged(ch, removed)`

Each new arm therefore needs only the thin `…AsAccount` adapter following the established pattern (`go/internal/comms/agent_caller.go:187-200`, `UpdatePinnedBoardAsAccount`: guard `account == ""` → `errNoActor`, then `c.UpdatePinnedBoard(WithActor(ctx, account), connect.NewRequest(req))`), plus a `CommsCaller` interface method and a `executeCall` dispatch case.

**create-Manager is a field-add on spawn, not a new arm.** The whole spawn pipeline already threads role and persona — only the request-side hardcode blocks it:

> `go/server/lifecycle.go:180-189`: "Persona and role are server-authoritative and empty on spawn (SpawnPeerRequest carries neither): the new account is created with no persona and no role … `created, err := l.store.CreateAgent(ctx, callerOwner, store.NewAgent{ Handle: req.GetHandle(), DisplayName: req.GetDisplayName(), Persona: "", Role: "", … })`"
>
> `go/internal/store/accounts.go:239-241`: `"INSERT INTO agent_accounts (account_id, owner_user_id, home_channel_id, persona, role, parent_agent_id) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))", accountID, string(ownerUserID), channelID, a.Persona, a.Role, string(a.ParentAgentID)` — the store already accepts both verbatim.
>
> `go/server/lifecycle.go:345-357` (`provisionAndStart`): signature already carries `persona string, role string` and threads them to `ProvisionAgentWorkspaceRequest{ … Persona: persona, Role: role }`.
>
> `go/internal/store/inputs.go:27-32`: "Role is the agent's operator-set block-0 selector (RIG-1732 T10). Empty means no role (default OMP block-0) … Unlike Persona (an append overlay), the label selects `config/prompts/<role>/SYSTEM.md`, delivered as the container's customSystemPrompt."

So the change is: add optional `role` + `persona` fields to `SpawnPeerRequest`, and in `SpawnAsAccount` replace the two hardcoded `""` with `req.GetRole()` / `req.GetPersona()`. compass-agent's existing `agents_spawn_peer` tool gains `role` and `persona` args — no new tool, no new arm, no new relay path.

### Alternatives considered

- **New `create_agent` CommsCallRequest arm reusing `CreateAgentRequest`** (`proto/compass/v1/comms.proto:42`, "Create an agent account owned by the authenticated caller (D9)"). Rejected: `CreateAgent` mints an account but no container/session — the agent-facing "create a sub-Manager" intent is spawn (account + provision + start, `lifecycle.go:197` `provisionAndStart`). A second creation path would fork idempotency (`client_request_id` whole-chain dedup, `agent_gateway.proto:172`) and parent-edge semantics (spawn sets `ParentAgentID: caller`, `lifecycle.go:193`). DECIDED against per the batch brief.
- **A new `OrgCallRequest` envelope + `RelayOrgCall` RPC.** Rejected: channel ops are comms ops; `CommsCallRequest` is the established envelope for exactly this class (DL-049 sibling-call-family shape, `agent_gateway.proto:93`), and a new envelope forces a new Runner relay RPC for zero isolation benefit — the Runner is a pure forwarder either way.
- **Deferring `create_channel_group` to operator-only.** An earlier draft deferred it — no agent need had materialized, and namespace-node minting felt operator-shaped. **Reversed** (Matt ruled B, 2026-08-25): the no-human-clicks principle makes agent-standupability the default, not a later add — everything an org needs (channels, groups, sub-managers, subscriptions) must be agent-creatable, humans reserved to the security boundary. `create_channel_group` is now a first-class arm (see Approach + DL-new-3), and the deeper tree-collapse is filed as follow-up A (DL-new-4).
- **Role allowlist on spawn.** Not designed in — OQ-1 resolved NO allowlist (Matt confirmed the "agent acts as owning user, ACLs later" posture); role-assignment is open to any owner, guarded only by D9 owner-scope.
- **Reject a handle-conflict spawn whose `role`/`persona` differs (`CodeAlreadyExists`).** Rejected in favor of pure idempotency (§Semantics riders): the existing spawn contract is retry-safe idempotent on `client_request_id` and handle, and a completed-call retry must get its original answer (`lifecycle.go:319-321`). Erroring on a field mismatch would make a benign retry fail and leaks whether the handle exists; `role`/`persona` are set-at-creation-only instead, and the contract documents it.

### Proto contract (cross-lane deliverable — the agreed shapes)

All messages in `proto/compass/v1/agent_gateway.proto` unless noted. Payload messages are the **existing** `comms.proto` messages, reused verbatim (verified shapes quoted below).

**1. `CommsCallRequest` — three new arms (next free field numbers after `pin = 6`):**

```proto
message CommsCallRequest {
  string call_id = 1;
  oneof call {
    PostMessageRequest post = 2;
    ListMessagesRequest list = 3;
    GetRosterRequest roster = 4;
    SetAgentStatusRequest set_status = 5;
    UpdatePinnedBoardRequest pin = 6;
    CreateChannelRequest create_channel = 7;              // NEW
    UpdateChannelMembersRequest update_members = 8;       // NEW
    CreateChannelGroupRequest create_channel_group = 9;   // NEW
  }
}
```

**2. `CommsCallResult` — three new arms (next free after `pin = 7`; `error = 4` is the in-band failure variant, unchanged):**

```proto
message CommsCallResult {
  string call_id = 1;
  oneof result {
    PostMessageResponse post = 2;
    ListMessagesResponse list = 3;
    CommsCallError error = 4;
    GetRosterResponse roster = 5;
    SetAgentStatusResponse set_status = 6;
    UpdatePinnedBoardResponse pin = 7;
    CreateChannelResponse create_channel = 8;              // NEW
    UpdateChannelMembersResponse update_members = 9;       // NEW
    CreateChannelGroupResponse create_channel_group = 10;  // NEW
  }
}
```

**3. `SpawnPeerRequest` — two new fields, both REQUIRED at the tool/contract
level (field 3 is `reserved`/`initial_prompt`, `agent_gateway.proto:170-171`;
next free is 5):**

```proto
message SpawnPeerRequest {
  string handle = 1;
  string display_name = 2;
  reserved 3;
  reserved "initial_prompt";
  string client_request_id = 4;
  // NEW — REQUIRED. The block-0 selector for the spawned peer: the label
  // selects config/prompts/<role>/SYSTEM.md, delivered as the container's
  // customSystemPrompt (store/inputs.go:27-32). Proto3-optional string on the
  // wire; presence is enforced at the compass-agent tool schema (a role-less
  // spawn is rejected there, not on the wire). Matt ruled role required — an
  // agent with no role gets no SYSTEM.md, so the tool is useless without it.
  string role = 5;
  // NEW — REQUIRED. The spawned peer's stable working context, baked as a
  // system-prompt append-overlay on top of the role prompt (store/types.go:154-157):
  // the repos / projects / lanes the agent works out of — deliberately NOT the
  // specific issues it works (those churn; the persona stays stable). Matt
  // ruled persona required (comment 5f7a13b3): it is semantically load-bearing
  // context that role (a prompt-file selector) does not carry. Proto3-optional
  // string on the wire; presence enforced at the tool schema.
  string persona = 6;
}
```

`SpawnPeerResponse`, `LifecycleCallRequest`/`Result`, and the Runner-leg relay envelopes (`RelayCommsCallRequest{session_id, call}` / `RelayCommsCallResponse{result}`, `runner.proto:472-484`; `RelayLifecycleCallRequest/Response`, `runner.proto:494-506`) are **unchanged** — the same envelope messages ride both hops, so the new arms flow through the relay with zero relay-proto change.

**4. Reused payload messages — verified shapes (`proto/compass/v1/comms.proto:590-600, 622-652`, plus `ChannelKind` at `:276-280`), copy-pasteable for the TS tool schemas:**

```proto
message CreateChannelRequest {
  // Leaf name within the group, e.g. "coordination".
  string name = 1;
  // The channel group this channel belongs to; empty for an ungrouped,
  // owner-scoped channel.
  string group_id = 2;
  ChannelKind kind = 3;
  // Initial members party to the channel.
  repeated string member_account_ids = 4;
}
// ChannelKind (comms.proto:276-280) — the `kind = 3` field above. CHANNEL is the
// default; DM auto-widens to GROUP_DM as members are added (comms.proto:273-275).
enum ChannelKind {
  CHANNEL_KIND_CHANNEL = 0;
  CHANNEL_KIND_DM = 1;
  CHANNEL_KIND_GROUP_DM = 2;
}
message CreateChannelResponse {
  Channel channel = 1;
}

message UpdateChannelMembersRequest {
  // The channel to mutate.
  string channel_id = 1;
  // Accounts to add as members (join, read access).
  repeated string add_member_account_ids = 2;
  // Accounts to remove from membership.
  repeated string remove_member_account_ids = 3;
  // Members to mark subscribed (push opt-in); must be current or added members.
  repeated string subscribe_account_ids = 4;
  // Members to mark unsubscribed (read-only).
  repeated string unsubscribe_account_ids = 5;
}
message UpdateChannelMembersResponse {
  Channel channel = 1;
}

message CreateChannelGroupRequest {
  // Leaf name of the group, e.g. "matt".
  string name = 1;
  // Parent group; empty for a top-level group.
  string parent_group_id = 2;
  ChannelGroupVisibility visibility = 3;
}
// ChannelGroupVisibility (comms.proto:214-219). Zero value is OWNER (private to
// the owning user + its agents); SHARED is open to all. The store rejects a
// child more open than its parent (child ≤ parent, requireGroupCreateAuthz +
// the ceiling check, store/channels.go:46-51).
enum ChannelGroupVisibility {
  CHANNEL_GROUP_VISIBILITY_OWNER = 0;
  CHANNEL_GROUP_VISIBILITY_SHARED = 1;
}
message CreateChannelGroupResponse {
  ChannelGroup group = 1;
}
```

**5. New `CommsCaller` methods (`go/internal/runnerhub/hub.go:226-238`) — the hub's narrow surface each arm dispatches to:**

```go
CreateChannelAsAccount(ctx context.Context, account store.AccountID, req *compassv1.CreateChannelRequest) (*compassv1.CreateChannelResponse, error)
UpdateChannelMembersAsAccount(ctx context.Context, account store.AccountID, req *compassv1.UpdateChannelMembersRequest) (*compassv1.UpdateChannelMembersResponse, error)
CreateChannelGroupAsAccount(ctx context.Context, account store.AccountID, req *compassv1.CreateChannelGroupRequest) (*compassv1.CreateChannelGroupResponse, error)
```

**Semantics riders (contract-level, both lanes rely on these):**

- Errors are **in-band**: handler failures render as `CommsCallResult.error` (`CommsCallError{code, message}`, `agent_gateway.proto:136-139`) — a tool error, never a transport teardown.
- Authz collapses to the same codes a human gets: a group the agent cannot see → the `edgeError` mapping of the store sentinel (NotFound/PermissionDenied), mirroring "the agent never learns a channel it cannot see exists" (`agent_caller.go:129-130`).
- `create_channel` with empty `group_id` is VALID: "empty for an ungrouped, owner-scoped channel" (`comms.proto:625-626`) — so channel creation does not require a parent group even with `create_channel_group` deferred.
- **Agent-created channels carry no policy on the wire and are born OPEN/ownerless.** `CreateChannelRequest` has no `post_policy`/`owner` field, so agent-created channels take the store zero-value (OPEN, no owner) — identical to a human caller via the same handler (`store/channels.go:96-107` rejects incoherent policy/owner combos). The TS lane MUST NOT invent a policy arg.
- **Account-id source is the roster.** `create_channel.member_account_ids` and `update_members.{add,remove,subscribe,unsubscribe}_account_ids` take account IDs, but an agent addresses peers by handle (`@codename`). The bridge is the roster: `RosterEntry.agent_account_id` exists (`comms.proto:706`) but `compass_roster` does not surface it today, so the compass-agent T6 leg renders `agent_account_id` in the roster output (no proto change) to let the model map `@handle → id` when populating these tools. Wire stays on account IDs (matches the human UI path); handle→id resolution is a roster-render concern in the TS lane, not a server-side resolve.
- **Membership-add is caller-supplied and inherits the human-caller contract — no per-member visibility gate.** `create_channel.member_account_ids` and `update_members.add_member_account_ids` are not validated against the actor's visibility: the store augments (the actor is always added in, and members' owning users are pulled in transitively — `expandOwnerMembership`, `channels.go:198`) but does not reject an arbitrary `account_id`. So an agent can seed/add any account into a channel it authors or can see — identical to a human operator through the same handler, D9-bounded by channel visibility. This is not a new authz gap (it is the shared human-caller behavior), but the compass-agent prompt/tool lane should reason about it as a membership-widening surface; it is distinct from OQ-1's role-grant question (member-add, not role-assign).
- `SpawnPeerRequest.role`/`persona` are proto3-optional strings on the wire (empty = key omitted from the container env), but **both are REQUIRED at the compass-agent tool schema** (Matt ruled): a spawn missing either is rejected at the tool, not on the wire. There is no role-less/persona-less agent spawn through this tool.
- **`role`/`persona` are set-at-creation-only.** A spawn resolving to an **existing** handle is idempotent success under the **STORED** role/persona, ignoring the request's values — both non-create paths thread `existing.Agent.Persona`/`existing.Agent.Role` (`lifecycle.go:322-327` already-placed conflict returns the existing container with no field comparison; `lifecycle.go:331` unplaced-resume re-provisions under the stored values), never `req.GetRole()/GetPersona()`, and `SpawnPeerResponse` carries neither field back. So `agents_spawn_peer{handle:X, role:"manager"}` where `X` already exists role-less is a silent success with a role-less peer. Both lanes MUST document `role`/`persona` as "set when the peer is first created" — never "the role the peer runs under" (false on any retry/handle collision). Pure idempotency is intended here; the rejected alternative (reject a handle-conflict spawn whose `role` differs, `CodeAlreadyExists`) is noted under Alternatives.

## Plan

## Global Constraints

- **Proto discipline:** additive only — new oneof arms at the exact field numbers above; no renumbering, no reserved reuse. Regen via the repo's existing buf/gen task. The `CommsCallRequest`/`CommsCallResult`/`SpawnPeerRequest` **envelopes live only in internal gen** (`go/internal/gen/compass/v1/agent_gateway.pb.go`) — there is no `agent_gateway.pb.go` under `go/gen`; `relay_comms.go:25-26` imports both packages because the internal oneof arms wrap **payload** types from the public mirror (`agent_gateway.pb.go:167` `Post *v1.PostMessageRequest`, `v1` = `go/gen/compass/v1`). The reused `CreateChannelRequest`/`UpdateChannelMembersRequest` payloads are already public-gen (comms.proto, unchanged). Net: regen touches both trees, but only internal `agent_gateway.pb.go` gains arms.
- **Trust model:** no arm ever carries a caller identity; the account is hub-binding-resolved (`relay_comms.go:7-15`). Any new `…AsAccount` method guards `account == ""` → `errNoActor` (pattern: `agent_caller.go:136-138`).
- **Error rendering:** handler errors surface as in-band `CommsCallError` at the relay edge (existing `executeCall` → caller-stamps pattern), never Connect stream teardown.
- **Server-authoritative spawn inputs:** `role`/`persona` from the request are stored via `store.CreateAgent` and then threaded from the **created store account** (`lifecycle.go:197` passes `created.Agent.Persona, created.Agent.Role`), preserving the store-as-source-of-record flow.
- **Red-green (rule://red-green-testing):** each slice lands tests first, watches them fail, then the minimal green.
- **Lane marking:** tasks below are tagged `[compass-server]` (this repo: proto, gen, relay, comms impl, lifecycle, store — though store needs no change) or `[compass-agent]` / `[compass-runner]` (contract consumers; out of scope for this lane's implementation but part of this contract).

### T1 — Proto: new arms + spawn fields `[compass-server]`

Add `create_channel = 7` / `update_members = 8` / `create_channel_group = 9` to `CommsCallRequest.call`; `create_channel = 8` / `update_members = 9` / `create_channel_group = 10` to `CommsCallResult.result`; `role = 5` / `persona = 6` to `SpawnPeerRequest` — exactly the shapes above. Refresh the `CommsCallRequest`/`CommsCallResult` doc-comment arm enumerations ("A successful call sets post/list…", `agent_gateway.proto:117-118`) to include the three new arms. Regen.

Interfaces:

- Produces: `compassv1.CommsCallRequest_CreateChannel`, `compassv1.CommsCallRequest_UpdateMembers`, `compassv1.CommsCallRequest_CreateChannelGroup` (and `…internal` mirrors), `CommsCallResult_CreateChannel`, `CommsCallResult_UpdateMembers`, `CommsCallResult_CreateChannelGroup`, `SpawnPeerRequest.GetRole() string`, `SpawnPeerRequest.GetPersona() string`.
- Consumes: existing `CreateChannelRequest/Response`, `UpdateChannelMembersRequest/Response`, `CreateChannelGroupRequest/Response` (comms.proto, unchanged).
- Test: generated-code compile; buf lint/breaking passes (additive).

### T2 — comms: `CreateChannelAsAccount` + `UpdateChannelMembersAsAccount` + `CreateChannelGroupAsAccount` `[compass-server]`

In `go/internal/comms/agent_caller.go`, three adapters on the `UpdatePinnedBoardAsAccount` pattern (`agent_caller.go:187-200`): guard empty account → `errNoActor`, delegate `c.CreateChannel(WithActor(ctx, account), connect.NewRequest(req))` / `c.UpdateChannelMembers(...)` / `c.CreateChannelGroup(...)`, return `resp.Msg`. No home-channel defaulting: `create_channel` names no channel; `update_members` must name its channel explicitly (`channel_id`, like `pin`); `create_channel_group` names no channel and takes an optional `parent_group_id` (empty = top-level group).

Interfaces:

- `func (c *Comms) CreateChannelAsAccount(ctx context.Context, account store.AccountID, req *compassv1.CreateChannelRequest) (*compassv1.CreateChannelResponse, error)`
- `func (c *Comms) UpdateChannelMembersAsAccount(ctx context.Context, account store.AccountID, req *compassv1.UpdateChannelMembersRequest) (*compassv1.UpdateChannelMembersResponse, error)`
- `func (c *Comms) CreateChannelGroupAsAccount(ctx context.Context, account store.AccountID, req *compassv1.CreateChannelGroupRequest) (*compassv1.CreateChannelGroupResponse, error)`
- Tests (pgtest, beside existing `agent_caller` tests): agent creates channel in visible group → `Channel` returned + `ChannelChanged` emitted **AND the creating agent account is in the returned channel's member set** (founding membership — `store.CreateChannel`'s `expandOwnerMembership` adds the actor, `channels.go:198`; assert it, since it is what lets the Manager immediately `PostAsAccount` to the channel it just made); invisible group → same code a human gets; empty account → `errNoActor`; subscribe/unsubscribe round-trip via `UpdateChannelMembersAsAccount`; **agent creates a top-level group → `ChannelGroup` returned owned by the agent's owning user (`CreateChannelGroup` sets `owner_user_id` server-side, `comms.go:193-197`) + `ChannelGroupChanged` emitted; agent creates a nested group under a parent it can see → succeeds; under an unauthorized/unknown parent → same `ErrNotFound` a human gets (`requireGroupCreateAuthz`, no existence oracle); a child more open than its parent → `ErrInvalidArgument` (ceiling check)**.

### T3 — hub: `CommsCaller` methods + `executeCall` dispatch `[compass-server]`

Extend the `CommsCaller` interface (`hub.go:226-238`) with the three methods from the contract §5. Add three `executeCall` cases (`relay_comms.go:409-467`) on the `Pin` pattern: `case *compassv1internal.CommsCallRequest_CreateChannel: resp, err := h.comms.CreateChannelAsAccount(ctx, account, c.CreateChannel); …` wrapping into `CommsCallResult_CreateChannel`; same for `UpdateMembers` and `CreateChannelGroup`. Update the default-case error string's variant list ("no recognized variant set (post/list/roster/set_status/pin)", `relay_comms.go:469-472`) to include `create_channel/update_members/create_channel_group`. Update `fakeCommsCaller` (`helpers_test.go`).

Interfaces:

- Consumes: T1 generated arms, T2 methods.
- Tests: relay round-trip per arm (dispatch reaches the fake with the right account + request; result wraps the right oneof) — including the `create_channel_group` arm; unknown-session fail-closed unchanged; handler error → in-band `CommsCallError`.

### T4 — lifecycle: thread `role`/`persona` through spawn `[compass-server]`

In `SpawnAsAccount` (`go/server/lifecycle.go`, the `store.CreateAgent` literal ~lines 185-194): replace `Persona: ""` / `Role: ""` with `Persona: req.GetPersona()` / `Role: req.GetRole()`; rewrite the preceding "caller cannot inject" comment (the rationale inverts by design — see OQ-1/OQ-2, the D9 owner-acts model makes the caller's owner the authority). `provisionAndStart` (its signature already carries `persona string, role string`) and `store.CreateAgent` (validates handle/owner only, `accounts.go` INSERT accepts both verbatim) need **no** change — both already accept the values. (Line numbers are cited against a moving file; anchor on the named functions.)

Interfaces:

- Consumes: T1 `SpawnPeerRequest.GetRole()/GetPersona()`.
- Tests (extend existing spawn pgtests, pattern `service_placement_pgtest_test.go:966-968` `provisionRole`): spawn with `role: "manager"` → agent_accounts row has role `manager` AND Provision wire carries `Role: "manager"`; same for persona; empty role+persona → identical to today (wire-level regression); idempotent re-spawn with same `client_request_id` keeps stored values. **The operator `ProvisionAgentWorkspace` overwrite-from-store tests (`service_placement_pgtest_test.go:411-475`, `TestProvisionAgentWorkspaceOverwritesRoleFromStore` / `…ClearsRoleForNonAgentAccount`) stay green and MUST NOT change** — `SpawnAsAccount` sets role/persona at `CreateAgent`, and `provisionAndStart` still threads the STORE value onto the Provision wire, so store-as-source-of-record holds; an implementer must not "fix" those still-valid tests.

### T5 — runner relay leg `[compass-runner]` (contract consumer; out of this lane's scope)

No relay-proto change (`RelayCommsCallRequest.call` carries `CommsCallRequest` whole, `runner.proto:472-475`). The runner's comms gateway is a **pure verbatim forwarder with no switch over `CommsCallRequest.call`** (`gateway_test.go:163` pins "forwarded Call is not the verbatim request … (Runner is a pure forwarder)"), so the runner lane's real work is **regen only** — no dispatch case to add.

Interfaces: the §1/§2 arms; pure pass-through.

### T6 — TS transport + tools + prompt + roster render `[compass-agent]` (contract consumer; out of this lane's scope)

Four deliverables against this contract: (a) transport encode/decode for `create_channel`/`update_members`/`create_channel_group` arms + `role`/`persona` on spawn; (b) three new tools — `comms_create_channel` (args: `name`, `group_id?`, `kind?`, `member_account_ids?`), `comms_update_members` (args: `channel_id`, `add?`, `remove?`, `subscribe?`, `unsubscribe?`), and `comms_create_channel_group` (args: `name`, `parent_group_id?`, `visibility?`) — plus **REQUIRED** `role` and `persona` args on the existing `agents_spawn_peer` (the tool schema enforces both; a spawn missing either is rejected at the tool, per Matt's ruling); (c) prompt copy documenting the ops, the persona-content convention (stable working context — repos/projects/lanes — not per-issue), and that `role`/`persona` are set-at-creation-only (a re-spawn onto an existing handle keeps the stored values —…

Interfaces: §1-§4 of the proto contract verbatim; tool errors render the `CommsCallError{code, message}` in-band variant.

## Tasks

- [ ] T1 `[compass-server]` proto arms + spawn fields + regen (shapes §Proto contract)
- [ ] T2 `[compass-server]` `CreateChannelAsAccount` / `UpdateChannelMembersAsAccount` / `CreateChannelGroupAsAccount` + pgtests
- [ ] T3 `[compass-server]` `CommsCaller` extension (3 methods) + `executeCall` dispatch (3 arms) + relay tests
- [ ] T4 `[compass-server]` spawn `role`/`persona` thread-through (drop both hardcodes) + spawn pgtests
- [ ] T5 `[compass-runner]` relay regen-only (no relay-proto change, no dispatch case — runner is a pure forwarder)
- [ ] T6 `[compass-agent]` transport + `comms_create_channel` / `comms_update_members` / `comms_create_channel_group` tools + `agents_spawn_peer` REQUIRED `role`/`persona` args + `agent_account_id` in roster render + prompt

## Ledger delta

Ledger-impact: deferred to freeze. Do not edit DECISIONS.md in this PR (the ledger is relocating under PR #576); the caller applies these rows at freeze:

- **DL-new-1:** Agent org-management ops are new `CommsCallRequest`/`CommsCallResult` oneof arms reusing the existing CommsService messages verbatim, executed under the D9 server-resolved caller (no new envelope, no relay-proto change).
- **DL-new-2:** create-Manager is a `role` (+ `persona`) field-add on `SpawnPeerRequest`, not a new create-agent arm — spawn remains the single agent-facing agent-creation path.
- **DL-new-3:** `create_channel_group` is an agent-exposed `CommsCallRequest`/`CommsCallResult` arm (reusing `CreateChannelGroupRequest`/`Response` verbatim), D9-bounded by the store's existing `requireGroupCreateAuthz` — agents stand up their own namespace groups, not just channels within pre-existing ones (Matt ruled B, 2026-08-25, under the no-human-clicks principle; supersedes the prior operator-only deferral).
- **DL-new-4:** The channel-group tree and the agent tree (`channel_groups.parent_group_id` / `agent_accounts.parent_agent_id`) are to be **collapsed into one hierarchy** — tracked as follow-up **RIG-2684** with its own design + migration (Matt-directed "A", 2026-08-25), explicitly NOT in this record's scope; it reshapes DL-135 (roster/tree) and DL-190 (coordination + brief-DM channel homes).
- **DL-new-5 (no-human-clicks — the mesh-internal principle):** The whole org structure — accounts, agents, channels, channel groups, subscriptions — MUST be standupable by agents through tools; the only human-reserved surface is the **security boundary**. An agent may DECLARE a secret by NAME; a human's sole step is providing the secret VALUE for that named slot. Any org operation that requires a human to click a console or run a one-off command is a defect to design out. This is the mesh-internal twin of the repo's IaC/merge-to-apply "no human clicks" infra posture (Matt, 2026-08-25); the org-mgmt arms in this record (create_channel / update_members / create_channel_group + spawn-with-role) exist to satisfy it. Captured first-class in the agent-facing docs (RIG-2680).

## Open Questions

All three OQs are **resolved** — Matt ruled OQ-1 and OQ-2 on RIG-2673 (comment `5f7a13b3`, 2026-08-24); OQ-3 was resolved in-design.

- **OQ-1 (role allowlist) — RESOLVED: NO allowlist.** Any owner may spawn a role-bearing agent; role-assignment is not gated by an operator allowlist. Matt confirmed the standing "agent acts as owning user, ACLs later" posture (RIG-2672). The role label only selects `config/prompts/<role>/SYSTEM.md` (`store/inputs.go:27-32`) under the caller's own owner (D9) and a nonexistent role directory grants nothing, so the blast radius is prompt selection, not privilege escalation. (Matt: "say the word if you want role-assignment reserved to the human path" — not requested; proceeding open.)
- **OQ-2 (persona required-vs-optional) — RESOLVED: BOTH role and persona REQUIRED.** Matt ruled persona required: it carries the agent's stable working context (the repos / projects / lanes it works out of, NOT the churning per-issue detail), which is semantically load-bearing and which `role` (a prompt-file selector) does not provide. Both are proto3-optional strings on the wire but required at the compass-agent tool schema. This voids the prior draft's designed-against persona-optional stance.
- **OQ-3 (parent group on create_channel) — RESOLVED in-design.** `CreateChannelRequest.group_id` is explicitly optional: "empty for an ungrouped, owner-scoped channel" (`comms.proto:625-626`), so an agent can create channels with or without a parent group. Note the prior deferral of `create_channel_group` is itself now lifted (Matt ruled B — see Approach + DL-new-3): agents create both channels and the groups that contain them.
