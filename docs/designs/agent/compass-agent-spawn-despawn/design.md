# Compass: agent-facing spawn/despawn of peer agents

Status: Active

Design for the wave's defining capability: a supervisor agent spawns and
despawns peer agents at runtime, through a Compass tool. Container create +
teardown are fully wired **operator-facing**; there is **no agent-facing
path** — no RPC an agent can reach, no tool. This record designs all three
seams: the agent→Server lifecycle relay (spawn + despawn), the missing
`RemoveAgentWorkspace` teardown RPC (absent even operator-side), and the
agent-side tool pair. Companion records this composes with:
[`compass-agent-comms-tools/design.md`](../compass-agent-comms-tools/design.md)
(the agent-tool + relay pattern this mirrors, DL-028/DL-029),
[`compass-server-ownership-layer/design.md`](../../product/compass-server-ownership-layer/design.md)
(#995 — the Server as ownership layer; its DL-049 sibling-call-family precedent),
and [`compass-agent-container-runtime.md`](../compass-agent-container-runtime.md)
(the provisioning substrate the spawn drives).

**Grounding.** This record and the code it describes now live together in this
repository, so citations resolve against this tree directly: `docs/…` and
sibling design records for records; `proto/…`, `go/…`, `packages/…` for the
code. All compass citations verified 2026-07-30 against the `compass` worktree
at `~/agents/workspaces/compass-server/compass`.

**Ownership contract (F2, ruled by Matt 2026-07-29 — designed against, not
relitigated).** A spawned agent is owned by the **spawning agent's owner**
(the human): all wave agents share the human `OwnerUserID`; the supervisor
spawns *on behalf of* its owner and gets spawn/despawn authority over its
wave. This is the simplest extension of `AgentAccount.OwnerUserID`.

## Problem / Intent

A supervisor agent cannot spawn or despawn peers today: the container
lifecycle (`ProvisionAgentWorkspace` → `StartAgentSession` →
`StopAgentSession`) is operator-only, there is no `RemoveAgentWorkspace` RPC
at all, and no agent tool reaches any of it. This record adds an owner-scoped
agent-facing spawn/despawn path — the capability the whole multi-agent wave
runs on.

## Approach

### The built substrate this composes with

Every seam this record adds is an extension of a chain that already exists
end-to-end for the operator. Verified at source:

- **Create is fully wired operator-facing.** `CompassService` (compass.proto:14)
  carries `rpc ProvisionAgentWorkspace(…)` (compass.proto:31),
  `rpc StartAgentSession(…)` (compass.proto:36), `rpc StopAgentSession(…)`
  (compass.proto:41), `rpc ReloadAgentSession(…)` (compass.proto:47). The
  server handler relays to the owning Runner — "ProvisionAgentWorkspace
  creates the isolated per-agent container … by relaying to the owning Runner
  (Client -> Server -> RunnerHub -> Runner -> AgentRuntime façade); the Server
  holds no container-engine code" (`go/server/service.go:100-103`) — via
  `func (h *Hub) Provision(ctx context.Context, requestID string, req
  *compassv1.ProvisionAgentWorkspaceRequest)
  (*compassv1.ProvisionAgentWorkspaceResponse, string, error)`
  (`go/internal/runnerhub/commands.go:48`), down the Runner's dispatch
  (`case *compassv1internal.SessionsResponse_Provision: containerName, err :=
  d.host.Provision(ctx, c.Provision)`, `go/internal/runner/dispatch.go:153-154`)
  into the runtime façade (`func (r *AgentRuntime) Launch(ctx context.Context,
  spec AgentSpec) (*AgentHandle, error)`, `go/internal/runtime/agent.go:152`),
  which really drives podman (`podman rm --force` at
  `go/internal/runtime/podman.go:483`, `podman stop` at `:470-473`).
- **Teardown exists at the runtime layer but has no container-remove RPC.**
  `func (r *AgentRuntime) Teardown(ctx context.Context, handle *AgentHandle)
  error` stops and removes the container (`go/internal/runtime/agent.go:191-204`)
  — but nothing calls it from any RPC. The Runner host is explicit that the
  seam is missing: "there is no per-container Deprovision RPC (a session
  Stop/Reload reuses the container and its socket), so every container lives
  until the Runner process ends" (`go/internal/runner/host.go:164-166`).
  `StopAgentSession` kills only the in-container agent exec
  (`func (h *agentHost) Stop(_ context.Context, sessionID string) error`,
  `go/internal/runner/host.go:241`); the container and its socket survive.
- **The ownership model is already `OwnerUserID`.** `func (s *Store)
  CreateAgent(ctx context.Context, ownerUserID AccountID, a NewAgent)
  (Account, error)` (`go/internal/store/accounts.go:131`) inserts
  `"INSERT INTO agent_accounts (account_id, owner_user_id, home_channel_id)
  VALUES ($1, $2, $3)"` (`accounts.go:156-158`) and returns
  `Agent: &AgentAccount{OwnerUserID: ownerUserID, …}` (`accounts.go:190-193`).
  On the wire, `AgentAccount.owner_user_id` is "Server-set to the creating
  caller; not a client-chosen field" (`proto/compass/v1/comms.proto:131-132`),
  and `rpc CreateAgent` is documented "The owner is the caller, not a request
  field — a user creates agents they own" (`comms.proto:40-42`).
- **Agent-initiated calls already have a trust model and a relay shape.** The
  agent dials a per-container Unix socket (`AgentGateway`,
  `proto/compass/v1/agent_gateway.proto:47-64`, DL-015/DL-017); the Runner
  forwards to the Server as a pure forwarder. The frozen trust model: "The
  Runner is a pure forwarder: it sends RelayCommsCall{session_id, call} and
  asserts NO account. The SERVER resolves session_id -> agent account from
  THIS hub's own binding — recorded from the Provision request's
  agent_account_id, promoted to the minted session_id at Start"
  (`go/internal/runnerhub/relay_comms.go:7-11`). The binding is fail-closed:
  "An entry exists only while the session is live: Start adds it, Stop
  removes it, and a Runner reconnect (enroll re-attach) drops ALL of them"
  (`go/internal/runnerhub/hub.go:166-168`); `accountForSession` returns
  "false when no live binding exists … the fail-closed signal RelayCommsCall
  turns into CodeNotFound" (`relay_comms.go:75-77`). A hub with no caller
  wired fails `CodeUnavailable` (`errCommsUnavailable`, `relay_comms.go:88`).
- **The admin-gate / fail-closed authz posture to mirror.** `IssueToken`
  (compass.proto:74) is admin-gated on the network door: "case adminOnly:
  caller, ok := CallerFrom(ctx); if !ok || caller != g.admin { return
  connect.NewError(connect.CodePermissionDenied, errAdminOnly) }"
  (`go/internal/auth/admin_gate.go:150-154`). Operator identity is
  connection-borne, read via `CallerFrom(ctx) (store.AccountID, bool)`
  (`go/internal/auth/interceptor.go:44-47`); a missing caller fail-closes
  (`errNoCaller`, `go/server/service.go:56-58`).
- **The agent-tool precedent.** The comms tools are native `AgentTool`s built
  by `export function createCommsTools(broker: CommsBroker): AgentTool[]`
  with `name: "comms_post_message"`, `approval: "write"`
  (`packages/compass-agent/src/comms.ts:255-259`), registered at agent boot
  (DL-028: "two native tools … no ask-answering capability"). The ownership
  layer record already extended this pattern once: DL-049 — "Forge tools ride
  the existing `AgentGateway` socket as a sibling `ForgeCall*` family relayed
  by `RelayForgeCall`, not a `CommsCallRequest` extension" (DECISIONS.md).
- **Durable placement + the missing delete path.** Provision records "the
  agent's durable PLACEMENT — which Runner it is on and the container name it
  runs under" via `s.store.RecordAgentPlacement(ctx, …)`
  (`go/server/service.go:118-133`); the store itself flags the gap this
  record closes: the placement has no "release path: that change needs a
  delete path added alongside it" (`go/internal/store/agent_placements.go:54`).

### Seam (a) — spawn: a `LifecycleCall` family on the agent gateway, orchestrated server-side

Spawn is a **sibling call family on the existing `AgentGateway` socket**,
exactly the DL-049 shape: a new `rpc Lifecycle(LifecycleCallRequest) returns
(LifecycleCallResult)` beside `rpc Comms(…)` (`agent_gateway.proto:48`), a new
Runner→Server unary `rpc RelayLifecycleCall(RelayLifecycleCallRequest)
returns (RelayLifecycleCallResponse)` beside `RelayCommsCall`
(`runner.proto:91`) — additive to the frozen dial-out model (the Runner still
initiates; the Server gains no inbound route, `runner.proto:35-37`). There is
deliberately **no public agent-callable spawn RPC on `CompassService`**: the
agent container holds no server token and no network path to the Server
(the egress-sealed container dials only its per-container socket,
`agent_gateway.proto:8-10`), so the relay IS the agent-facing RPC edge.

Wire shapes (held-for-review proto delta — see Global Constraints):

```proto
// agent_gateway.proto — agent -> Runner, unary, beside Comms.
message LifecycleCallRequest {
  string call_id = 1;                 // agent-minted correlation id (SDK toolCallId)
  oneof call {
    SpawnPeerRequest spawn = 2;
    DespawnPeerRequest despawn = 3;
  }
}
message SpawnPeerRequest {
  string handle = 1;                  // the new agent's account handle (unique)
  string display_name = 2;
  string initial_prompt = 3;          // threaded to StartAgentSessionRequest.initial_prompt
  string client_request_id = 4;       // whole-chain idempotency key (handler-level join + Provision dedup)
}
message SpawnPeerResponse {
  string agent_account_id = 1;
  string container_name = 2;
  string session_id = 3;
}
message DespawnPeerRequest {
  string agent_account_id = 1;        // the peer to tear down
}
message DespawnPeerResponse {}
message LifecycleCallResult {
  string call_id = 1;
  oneof result {
    SpawnPeerResponse spawn = 2;
    DespawnPeerResponse despawn = 3;
    LifecycleCallError error = 4;     // in-band tool error {code, message},
  }                                   // same shape as CommsCallError
}
message LifecycleCallError {
  string code = 1;
  string message = 2;
}

// runner.proto — an additive Runner-initiated unary, sibling to
// RelayCommsCall/RelayForgeCall (the seventh RPC once the ownership-layer's
// RelayForgeCall lands; five exist today, runner.proto:55-117).
rpc RelayLifecycleCall(RelayLifecycleCallRequest) returns (RelayLifecycleCallResponse);
message RelayLifecycleCallRequest {
  string session_id = 1;              // the session the Runner structurally owns
  LifecycleCallRequest call = 2;      // the agent's request verbatim
}
message RelayLifecycleCallResponse {
  LifecycleCallResult result = 1;
}
```

**Spawn carries no repo (Matt, 2026-07-29).** `SpawnPeerRequest` names the
peer's identity and its initial prompt — never a repo/ref to clone. The
container is provisioned with a git credential and a workspace; which repos
exist inside it is the agent's business, so a spawned peer clones whatever it
has credentials for, after launch. This mirrors the same removal on
`ProvisionAgentWorkspaceRequest` (server-side auto-clone is deleted, not made
optional) — see the ownership record's OQ-4, superseded to *removed*
([`compass-server-ownership-layer/design.md`](../../product/compass-server-ownership-layer/design.md),
OQ-4). Post-MVP, an agent tool may clone on the agent's behalf so agents need
hold no git creds; until then the container's scoped credential stands.

**Server-side orchestration, one call — in the server package, not the
hub.** The spawn handler executes the full create chain server-side rather
than exposing the three operator steps to the agent: `store.CreateAgent`
(new account, owner inherited — below) → `hub.Provision` (pushes the
provision command down the same Runner's `Sessions` stream; single-Runner
MVP places the peer on the caller's Runner, "any non-empty key resolves the
one Runner", `commands.go:129-130`) → `store.RecordAgentPlacement` →
`hub.Start` → `store.RecordAgentSession` + session-binding promotion — the
exact writes the operator handlers make today (`service.go:133`,
`:178-186`; `commands.go:57-61`, `:77-81`). The orchestration lives in a
**server-package `LifecycleCaller`** injected into the hub, mirroring how
execution is delegated for comms and forge: the hub "depends only on this
narrow surface so it does not pull the whole CommsService in"
(`CommsCaller`, `go/internal/runnerhub/hub.go:123-124`), and the
ownership-layer's `ForgeCaller` repeats the shape ("declared here so the
dependency runs one way (runnerhub -> forge) and the hub never imports a
forge client", its design.md:1910-1911). The hub's relay handler does only
what it is competent at — resolve `session_id → caller`, fail-closed — and
delegates spawn/despawn execution to the seam: the hub gains no store
dependency, and the placement/session invariants stay in one place beside
`service.go`'s existing Provision/Start bookkeeping (`service.go:118-135`,
`:158-186`) instead of being duplicated into the hub.

**Serial command plane: no deadlock, but head-of-line blocking.** There is
no deadlock — verified goroutine-by-goroutine: the relay unary arrives on
its own HTTP call, distinct from the `Sessions` bidi stream the commands
ride down. But the Runner executes session commands **strictly serially**:
`RunSessions` receives one command, handles it inline, and sends the result
before receiving the next (`cmd, err := stream.Receive()` → `result :=
d.handle(ctx, cmd)` → `stream.Send(result)`,
`go/internal/runner/dispatch.go:104-112`), and `d.handle` runs
`host.Provision` inline. A spawn's minutes-long container build/pull
therefore head-of-line-blocks every other command on that Runner — Stop,
Status, despawn, a second spawn, an operator reload (OQ-6). The mitigation
this record ships regardless of OQ-6's answer: a **bounded deadline on the
relay chain** — the gateway→Runner→Server spawn chain runs under a deadline
so a wedged provision fails the tool call in-band instead of parking
forever, the posture `abandonStartedSession` already takes with
`rollbackStopTimeout` ("The runnerhub dispatch path (internal/runnerhub)
has NO deadline of its own", `service.go:43-46`).

**Failure posture: whole-chain idempotency first, rollback second.** Spawn
is multi-step and not atomic (CreateAgent → Provision →
RecordAgentPlacement → Start → RecordAgentSession); a response can be lost
at any point mid-chain. The PRIMARY idempotency is a **whole-chain join on
`client_request_id` at the lifecycle handler**, mirroring the router's
pendingCall semantics one layer down: "A retry with a live request id joins
the existing call rather than issuing a second command"
(`go/internal/runnerhub/router.go:34-40`); "Multiple retriers of the same
request id all wait on the same pendingCall, so they observe one identical
outcome" (`router.go:46-53`). A retried spawn with the same
`client_request_id` joins the in-flight (or just-completed) call and
receives the SAME `SpawnPeerResponse` — collapsing the mid-chain window
where a lost response after `RecordAgentPlacement` would otherwise strand
the caller with neither `session_id` nor `agent_account_id` in hand.
`client_request_id` still threads into the provision dedup
(`provisionDedupID`, `commands.go:172-180`), so the Provision leg is doubly
covered.

On a Provision/Start failure after `CreateAgent` succeeded, the partial
container/session is torn back down best-effort (the
`abandonStartedSession` pattern, `service.go:369-383`) **and the rollback
calls `store.DeleteAgentPlacement`**, so a failed spawn does not burn the
handle: the created account persists (accounts have no delete path; the
home channel and any history stay coherent) but is left **unplaced**, and a
later spawn of the same handle proceeds. Resume-by-handle is the
**crash-recovery path only** (a Server crash mid-chain loses the in-memory
join), and it is **owner-checked** via `store.AgentByHandle` (T2): the only
handle lookup today is the private, admin-asserting `adminByHandle`
(`accounts.go:101-105`), and `CreateAgent` surfaces a duplicate as a bare
`ErrConflict` without the existing account (`accounts.go:151-152`), so
neither supports the check — a caller must never resume onto a same-handle
agent owned by someone else (the D9 posture despawn takes). A same-owner
handle with **no live placement** resumes idempotently; a handle owned by a
different owner, or one that exists **with** a placement, is a genuine
in-band `already_exists`.

### Identity and authz — owner inherited from the caller, resolved at the RPC edge

The caller agent's identity is **never a request field**. The chain, all
existing machinery:

1. The Runner door authenticates the Runner subject and fail-closes anything
   else — the `RelayCommsCall` handler's guard is reused verbatim for the new
   relay: "if _, ok := runnerSubjectFrom(ctx); !ok { return nil,
   errUnauthenticated }" (`go/internal/runnerhub/handler.go:132-135`).
2. The hub resolves `session_id → caller agent AccountID` via
   `accountForSession` (`relay_comms.go:78`), fail-closed `CodeNotFound` for
   an unbound/stopped/reconnect-dropped session.
3. The lifecycle handler resolves `caller agent → OwnerUserID` via
   `store.GetAccount` (`accounts.go:201`), reading
   `Account.Agent.OwnerUserID` — the field `CreateAgent` set at creation
   (`accounts.go:190-193`). A caller that is not an agent account (impossible
   via this path, checked anyway) fail-closes.
4. **Spawn**: the new peer is created with `store.CreateAgent(ctx,
   callerOwnerUserID, …)` — the F2 ruling realized in one argument. The
   supervisor's owner owns the whole wave.
5. **Despawn**: authority is **same-owner**. The handler resolves the target
   `agent_account_id`'s `OwnerUserID` and requires it equal the caller's.
   An unknown target and an other-owner target return the **same** in-band
   `not_found` (the D9 not-found/forbidden merge the codebase already uses:
   "returning the SAME not-found for an unknown session and a non-member so
   neither can probe … existence", `service.go:317-320`). A target that is a
   user account is `not_found` too. **Self-despawn is refused**
   (`invalid_argument`): tearing down the caller's own container mid-call
   would kill the session the response must return on; stopping oneself is
   the operator's `StopAgentSession` lane.

**Failure mode: control-plane cliff on rebind.** The session→caller binding
is in-memory only: "a Runner reconnect (enroll re-attach) drops ALL of
them" (`hub.go:166-168`), and a Server restart loses them outright. After
either, every spawn/despawn call from a still-running wave fails closed
`CodeNotFound` until Runner-side session resume rebinds — mid-wave, the
wave's control plane is unmodifiable by any agent (operator lane only).
This is the ruled comms posture inherited unchanged, but the blast radius
differs: for comms the cost is one dropped frame; for lifecycle it is the
whole wave's spawn/despawn capability. The surfaced `not_found` is also
confusable — the caller cannot tell "target missing" from "your own session
is unbound"; the handler knows which lookup failed, so the MVP surfaces a
distinct in-band code for the caller-session-unbound case (cheap, since the
handler already distinguishes the two lookups), tracked in **RIG-1578**.
The durable-placement fallback is deliberately NOT
taken: resolving the caller from `agent_placements` instead of the live
binding would reopen the misattribution enroll's `clear()` exists to close
("clearing is what stops the new session's words being attributed to the
previous account", `hub.go:383-386`).

### Seam (b) — despawn + the new `RemoveAgentWorkspace` RPC

Despawn = stop the live session (idempotent, existing semantics: "stopping an
unknown/already-stopped session succeeds", `service.go:190-193`), **remove the
container**, and release the durable records. The container removal needs the
RPC that does not exist anywhere today; this record adds it at every layer,
operator-facing first (the agent path composes it):

- **Public RPC** on `CompassService`, admin-gated exactly like `IssueToken`
  (the AdminGate classifies it `adminOnly`; the agent-facing despawn never
  touches this door — it rides the relay with its own owner-scoped check):

  ```proto
  // compass.proto — symmetric with ProvisionAgentWorkspace (the container it
  // tears down): keyed by container_name, same idempotency contract.
  rpc RemoveAgentWorkspace(RemoveAgentWorkspaceRequest) returns (RemoveAgentWorkspaceResponse);
  message RemoveAgentWorkspaceRequest {
    string container_name = 1;
    string client_request_id = 2;   // idempotency key, same retry contract as Provision
  }
  message RemoveAgentWorkspaceResponse {}
  ```

- **Sessions relay command**: a new variant in the frozen command/result
  oneofs — `RemoveAgentWorkspaceRequest remove = 10;` in
  `SessionsResponse.command` and `RemoveAgentWorkspaceResponse remove = 8;`
  in `SessionsRequest.result` — reusing the public payload verbatim, the
  relay's stated convention ("The command variants reuse the frozen public
  session-RPC request payloads (compass.proto) verbatim",
  `runner.proto:157-159`). The tags are allocated by compass-repo (the
  single proto-owner) against the cross-record reservation ledger, not read
  off the live proto — see Global Constraints.
- **Runner execution**: a new `agentHost.Remove(ctx, containerName)`:
  retire any live session bound to the container (the `Stop` path,
  `host.go:241-260`), `runtime.Teardown(handle)` (stop + `rm --force`,
  `agent.go:191-204` → `podman.go:470-484`), close the per-container socket
  (`closeSocket`, the leak-avoidance path Provision already uses,
  `host.go:137-140`). Idempotent: an unknown container name succeeds (the
  Stop semantics extended to Remove).
- **Server bookkeeping on success**: `store.DeleteAgentPlacement` (the
  flagged missing "delete path", `agent_placements.go:54`) and dropping the
  hub's live container/session bindings so a late comms call from the
  removed agent fails closed `CodeNotFound` (the same posture Stop already
  takes: "Drop the session's account binding … never a stale reuse",
  `commands.go:94-97`).
- **The agent account persists.** Despawn tears down compute, not identity:
  the account row, home channel, and message history stay (messages
  reference the author account; deleting it would orphan them). Respawning
  the same peer later is a fresh spawn against the surviving account
  (the no-live-placement resume above).

### Seam (c) — the agent-side tools

Two native `AgentTool`s mirroring the comms pair (`comms.ts:255-259`),
registered at agent boot alongside `createCommsTools` (DL-028's
non-serializable-`execute` rationale applies unchanged):

- `agents_spawn_peer` — `approval: "write"`. Params: `handle`,
  `display_name?`, `initial_prompt?`. Returns the spawned peer's `agent_account_id`,
  `container_name`, `session_id` rendered as text. The broker mints
  `client_request_id` per logical call — derived from the SDK `toolCallId`
  via the comms broker's nonce-scoped `idempotencyKey` pattern
  (`comms.ts:93-96`; see T6) — so SDK-level retries dedupe.
- `agents_despawn_peer` — `approval: "write"`. Params: `agent_account_id`.

The transport seam gains `lifecycle(call): Promise<LifecycleCallResult>`
beside the existing `comms()` on `CommsTransport`
(`packages/compass-agent/src/comms.ts`, re-exported at
`packages/compass-agent/src/index.ts:13`); a `LifecycleBroker` is the same
thin adapter shape as `CommsBroker` — no bespoke correlation, the Connect
unary response is the result. In-band `LifecycleCallError` renders as a tool
error the model reads; it never tears the transport down.

## Alternatives considered

- **Extend `CommsCallRequest` with spawn/despawn variants** — rejected: same
  reasoning as DL-049 (forge went sibling-family, not a `CommsCallRequest`
  extension); lifecycle is not comms, and the comms envelope's oneof is
  deliberately narrow ("The request oneof … cannot express RespondToAsk — the
  no-answer-ask constraint is structural", `runner.proto:88-90`; widening it
  for lifecycle would dilute that structural property).
- **A public agent-callable `SpawnAgent` on `CompassService`** — rejected: the
  agent has no token and no network path to the Server; every agent-initiated
  call rides the socket→Runner→relay chain by frozen decision (DL-015/DL-017).
  A public RPC would exist only for a caller that cannot reach it.
- **Expose Provision/Start/Stop individually to the agent** (three relay
  variants instead of one orchestrated SpawnPeer) — rejected: it forces the
  agent to carry the Server's bookkeeping invariants (placement write between
  Provision and Start, session recording, rollback-on-partial-failure —
  `service.go:118-135`, `:158-186`) which the Server must own; a
  half-executed sequence from a crashed agent turn would strand containers.
- **Hub-side spawn/despawn orchestration** (the hub holding a
  `LifecycleStore` and making the CreateAgent/placement/session writes
  itself) — rejected: it duplicates `service.go`'s placement/session
  invariants (`service.go:118-135`, `:158-186`) across two packages — a
  second copy that would drift (a RIG-1516 reattach change to the
  server-side write would not reach the hub copy) — and widens the hub
  beyond its stated scope. The `CommsCaller`/`ForgeCaller` precedent
  delegates execution to the domain package and leaves the hub only
  resolution ("the hub depends only on this narrow surface so it does not
  pull the whole CommsService in", `hub.go:123-124`).
- **Despawn deletes the agent account** — rejected: no account delete path
  exists, messages/home-channel reference it, and history integrity is a
  comms invariant. Compute and identity have different lifetimes.

## Global Constraints

Every task below inherits these; they are not repeated per task.

- **F2 ownership ruling is the fixed frame.** Spawned peer owned by the
  spawning agent's owner; despawn authority is same-owner scoped. No task may
  introduce an agent-owns-agent or operator-only variant.
- **Go module root is `go/`** (the compass repo's Go module lives under the
  `go/` subdirectory; all Go paths below are relative to it as `go/…`).
- **The proto delta is a HELD-FOR-REVIEW schema change owned by the
  compass.v1 owner.** This record specifies the message/RPC shapes; the
  actual `.proto` edit rides the implementation PR **after** that review —
  no task writes proto ahead of it. All three touched files
  (`agent_gateway.proto`, `runner.proto`, `compass.proto`) are additive
  (new RPCs, new messages, new oneof variants at fresh tags), so
  `buf breaking` passes. **Oneof tags are coordinated, not read off the
  live proto.** Four held-for-review variants converge on
  `SessionsResponse.command` (in-proto today: 2–6, `runner.proto:164-170`):
  the ownership-layer record's `forge_notification = 7` (its
  design.md:1633), RIG-1327's `secrets_version = 8`, RIG-1568's
  `config_version = 9`, and this record's `remove = 10` — allocated by
  compass-repo (the single proto-owner) against that reservation ledger at
  the coordinated impl PR, because "next free off the live proto" misses
  reserved-but-not-yet-landed claims. Only `remove` needs a result variant
  (the others are signal-only Server→Runner pushes): `remove = 8` in
  `SessionsRequest.result` (in-proto today 2–7, `error = 7`,
  `runner.proto:145-152`). The internal-only files stay internal-gen-only
  (the RIG-1267 gen-fence: `LifecycleCall*` / `RelayLifecycleCall*` must be
  added to the fence grep alongside `CommsCall*`). `RemoveAgentWorkspace*`
  is deliberately **not** added to the fence: it is a PUBLIC
  `compass.proto` family (client-facing, like `ProvisionAgentWorkspace*`),
  so fencing it would be wrong — do not "fix" it in.
- **Fail-closed authz everywhere.** Unresolvable session → `CodeNotFound`;
  non-Runner subject on the relay → `CodeUnauthenticated`; unknown target,
  other-owner target, and user-account target on despawn → the same in-band
  `not_found` (no existence probe); missing hub/comms wiring →
  `CodeUnavailable`. Never a stale account, never the bootstrap admin, never
  a silent success.
- **Tool registration is native, at agent boot** — `AgentTool.execute` is
  non-serializable (DL-028), so the new tools register beside
  `createCommsTools`, never via `ConfigControl.tools`.
- **Naming**: tools are `agents_spawn_peer` / `agents_despawn_peer`
  (family-prefixed like `comms_*`); the call family is `LifecycleCall*` on
  the gateway and `RelayLifecycleCall` on `RunnerService` (sibling to
  `RelayCommsCall`/`RelayForgeCall`, DL-049 convention); the public RPC is
  `RemoveAgentWorkspace` (symmetric with `ProvisionAgentWorkspace`).
- **Idempotency**: spawn's PRIMARY idempotency is the whole-chain
  `client_request_id` join at the lifecycle handler (the router's
  pendingCall-join semantics one layer down, `router.go:34-53`);
  `client_request_id` also threads into the existing provision dedup
  (`provisionDedupID`, `go/internal/runnerhub/commands.go:172-180`).
  `RemoveAgentWorkspace` and despawn are idempotent by semantics (removing
  an absent container succeeds), mirroring `StopAgentSession`.
- **Red→green testing** (`rule://red-green-testing`): every task writes its
  failing test first. Gates: gofmt + golangci for Go, biome for TS,
  markdownlint for this record; `buf lint` / `buf breaking` for the proto
  delta when it lands.
- **Frozen-record convention**: merging this record ratifies the additive
  `RelayLifecycleCall` unary on `RunnerService` (sibling to
  `RelayCommsCall`/`RelayForgeCall` — the seventh RPC once the
  ownership-layer's `RelayForgeCall` lands; five exist today,
  `runner.proto:55-117`) and the new `CompassService` RPC; later changes
  add a new record.

## Plan

Ordered so each task lands green on its own; T1 (proto) is the held-for-review
delta the rest builds against.

### T1 — proto delta: lifecycle call family + `RemoveAgentWorkspace`

The full schema change, one reviewable unit, exactly the shapes in the
Approach:

- `agent_gateway.proto`: `rpc Lifecycle(LifecycleCallRequest) returns
  (LifecycleCallResult)` on `AgentGateway`; messages `LifecycleCallRequest`
  (`call_id`, oneof `spawn`/`despawn`), `SpawnPeerRequest`,
  `SpawnPeerResponse`, `DespawnPeerRequest`, `DespawnPeerResponse`,
  `LifecycleCallResult`, `LifecycleCallError`.
- `runner.proto`: `rpc RelayLifecycleCall(RelayLifecycleCallRequest) returns
  (RelayLifecycleCallResponse)`; `RelayLifecycleCallRequest{string
  session_id = 1; LifecycleCallRequest call = 2;}`,
  `RelayLifecycleCallResponse{LifecycleCallResult result = 1;}`; oneof
  variants `RemoveAgentWorkspaceRequest remove = 10;` in
  `SessionsResponse.command` and `RemoveAgentWorkspaceResponse remove = 8;`
  in `SessionsRequest.result` — tags per the coordinated allocation in
  Global Constraints (compass-repo assigns them at the impl PR across the
  four converging held variants).
- `compass.proto`: `rpc RemoveAgentWorkspace(RemoveAgentWorkspaceRequest)
  returns (RemoveAgentWorkspaceResponse)`;
  `RemoveAgentWorkspaceRequest{string container_name = 1; string
  client_request_id = 2;}`, `RemoveAgentWorkspaceResponse{}`.
- Extend the gen-fence grep with `LifecycleCall` + `RelayLifecycleCall`;
  regenerate internal Go + agent TS lanes.

Interfaces: the proto messages above, verbatim. Generated:
`compassv1internal.RelayLifecycleCallRequest/Response`,
`compassv1.RemoveAgentWorkspaceRequest/Response`, TS `LifecycleCallRequest`
et al. in `packages/compass-agent/src/gen`.

### T2 — store: placement release + reverse reads + owner resolution

The store reads/writes the handlers need.

- `DeleteAgentPlacement` — the release path `agent_placements.go:54` flags as
  missing. Idempotent: deleting an absent row succeeds.
- `PlacementForAgent` — the REVERSE placement read (agent → Runner +
  container). Despawn is keyed by `agent_account_id`, but `Remove` and
  `DeleteAgentPlacement` are keyed by `container_name`; the store today has
  only the forward `AgentForContainer(containerName)`
  (`agent_placements.go:94`) and the per-Runner list
  (`ListAgentPlacementsForRunner`, `agent_placements.go:118`), so despawn
  cannot resolve its target container without this read.
- `AgentOwner` — resolve an agent account's owner for the despawn authority
  check and spawn inheritance. (A thin projection over the existing
  `agent_accounts.owner_user_id` column, `accounts.go:156-158`; additive,
  per the ownership-layer rule that new resolvers never refactor existing
  predicates.)
- `AgentByHandle` — the owner-checkable handle lookup the crash-recovery
  resume path needs (Approach, failure posture). The only handle lookup
  today is the private, admin-asserting `adminByHandle`
  (`accounts.go:101-105`), which must not be reused: it asserts the account
  is an admin. `AgentByHandle` returns the full `Account` so the caller can
  owner-check; it never elevates.

Interfaces:

```go
// go/internal/store/agent_placements.go
func (s *Store) DeleteAgentPlacement(ctx context.Context, containerName string) error

// PlacementForAgent returns the Runner and container an agent is placed on,
// or ErrNotFound for an unknown or unplaced agent.
func (s *Store) PlacementForAgent(ctx context.Context, agentAccountID AccountID) (runnerID, containerName string, err error)

// go/internal/store/accounts.go
// AgentOwner returns the owning user of an agent account, or ErrNotFound
// when the id is unknown OR names a non-agent account (no existence probe).
func (s *Store) AgentOwner(ctx context.Context, agentAccountID AccountID) (AccountID, error)

// AgentByHandle returns the agent account with the given handle, or
// ErrNotFound (unknown handle, or a non-agent account). Unlike the private
// adminByHandle it never asserts or elevates; the caller owner-checks the
// returned account.
func (s *Store) AgentByHandle(ctx context.Context, handle string) (Account, error)
```

Red-first: pgtest table tests in `agent_placements_test.go` (delete releases
the container name for re-placement; absent row succeeds;
`PlacementForAgent` round-trips from `RecordAgentPlacement`;
unknown/unplaced agent → `ErrNotFound`) and `accounts_test.go` (owner
round-trips from CreateAgent; unknown id and user-account id both
`ErrNotFound`; `AgentByHandle` round-trips from CreateAgent; unknown handle
and user handle both `ErrNotFound`).

### T3 — runner: `Remove` on the session host + dispatch variant

- `agentHost.Remove(ctx, containerName)`: retire any live session bound to
  the container (reusing the `Stop` retirement under `h.mu`,
  `host.go:241-260`), resolve the handle via the registry,
  `runtime.Teardown` (`agent.go:191-204`), then `closeSocket`
  (`host.go:137-140`). Unknown container name → nil (idempotent). A handle
  the registry no longer resolves but whose socket file survives is cleaned
  up too (the crash-reclaim comment at `host.go:166-168`).
- Dispatch: `case *compassv1internal.SessionsResponse_Remove:` beside
  `_Provision`/`_Stop` (`dispatch.go:153-165`), answering
  `RemoveAgentWorkspaceResponse` under the command's `request_id`.

Interfaces:

```go
// go/internal/runner/host.go — added to the SessionHost interface + agentHost.
Remove(ctx context.Context, containerName string) error
```

Red-first: dispatch table test (`dispatch_test.go` pattern,
`dispatch_test.go:160-177`) + host test: Remove tears down a launched
container (fake ContainerRuntime records Stop+Remove), retires the bound
session, closes the socket; a second Remove is a no-op.

### T4 — runnerhub: `Remove` relay + `RelayLifecycleCall` resolution edge

The hub does only what it is competent at: relay the `remove` command, and
resolve the lifecycle caller's identity fail-closed. It gains **no store
dependency** — execution is delegated to the server-package
`LifecycleCaller` (T5), mirroring how `CommsCaller`/`ForgeCaller` are
injected into `NewHub` (`hub.go:211`).

- `Hub.Remove` — the Sessions-relay wrapper, same shape as `Hub.Stop`
  (`commands.go:86-99`): dispatch the `remove` command, drop the container's
  live account binding and any session binding bound to it (the fail-closed
  posture Stop takes, `commands.go:94-97`).
- `Hub.RelayLifecycleCall` — the agent-facing edge. Guard order (each
  fail-closed per Global Constraints): lifecycle caller wired
  (`CodeUnavailable` otherwise, the `errCommsUnavailable` pattern,
  `relay_comms.go:88`) → resolve `session_id → caller`
  (`accountForSession`, `relay_comms.go:78`) → delegate the call plus the
  resolved caller `AccountID` to the `LifecycleCaller`. Authz/tool errors
  come back in-band as `LifecycleCallError` (never a stream error);
  transport/infra failures are Connect errors, exactly the `RelayCommsCall`
  split (`relay_comms.go:90-131`).
- `sessionForAccount` — despawn must stop the target's live session, and
  the hub's binding map runs session→account (`sessionAccounts`,
  `hub.go:165-172`); add the small hub-internal reverse (iterate the
  bindings under `h.mu`), exposed to the `LifecycleCaller` seam. This is a
  hub-internal lookup, not a store read.
- `handler.go`: mount `RelayLifecycleCall` with the same
  `runnerSubjectFrom` guard (`handler.go:132-135`).

Interfaces:

```go
// go/internal/runnerhub/commands.go
func (h *Hub) Remove(ctx context.Context, requestID string, req *compassv1.RemoveAgentWorkspaceRequest) (*compassv1.RemoveAgentWorkspaceResponse, error)

// go/internal/runnerhub/relay_lifecycle.go (new file, sibling to relay_comms.go)
func (h *Hub) RelayLifecycleCall(ctx context.Context, req *compassv1internal.RelayLifecycleCallRequest) (*compassv1internal.RelayLifecycleCallResponse, error)

// go/internal/runnerhub/handler.go
func (h *Handler) RelayLifecycleCall(ctx context.Context, req *connect.Request[compassv1internal.RelayLifecycleCallRequest]) (*connect.Response[compassv1internal.RelayLifecycleCallResponse], error)

// LifecycleCaller executes an agent-initiated lifecycle call as a resolved
// caller account. The SERVER package implements it beside service.go's
// existing Provision/Start bookkeeping, so the hub never holds store
// writes (pattern: CommsCaller, hub.go:120-128; the ownership-layer's
// ForgeCaller). Injected via NewHub like the other callers; a nil
// LifecycleCaller fails CodeUnavailable.
type LifecycleCaller interface {
    SpawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
    DespawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.DespawnPeerRequest) (*compassv1internal.DespawnPeerResponse, error)
}
```

Red-first (the security tests, `relay_comms_test.go` pattern): unbound
session → `CodeNotFound`; nil `LifecycleCaller` → `CodeUnavailable` checked
before session resolution; the resolved CALLER account (never a
request-asserted one, never admin) reaches a fake `LifecycleCaller`; a
caller error surfaces in-band inside a successful response;
`sessionForAccount` resolves a bound session and misses an unbound one.

### T5 — server: lifecycle orchestration + `RemoveAgentWorkspace` handler + AdminGate row

- **The `LifecycleCaller` impl** lives in the server package beside the
  Provision/Start bookkeeping it reuses (`service.go:118-135`, `:158-186`)
  — one copy of the placement/session invariants (see Alternatives:
  hub-side orchestration rejected).
  - **Spawn** — under the whole-chain `client_request_id` join (a retry
    with a live id joins the in-flight or just-completed call and returns
    the same `SpawnPeerResponse`; the router's pendingCall shape,
    `router.go:46-53`): `store.CreateAgent(ctx, callerOwner,
    NewAgent{Handle, DisplayName})` → `Hub.Provision` (threading
    `client_request_id`) → `store.RecordAgentPlacement` → `Hub.Start` →
    `store.RecordAgentSession`. On `ErrConflict` from CreateAgent:
    `store.AgentByHandle` + owner check; same-owner with no live placement
    (`store.PlacementForAgent` → `ErrNotFound`) resumes; otherwise in-band
    `already_exists`. On partial failure after CreateAgent, abandon via the
    bounded-Stop rollback (`service.go:369-383`) AND
    `store.DeleteAgentPlacement` — the account is left unplaced, the handle
    not burned. The whole chain runs under a bounded deadline (Approach:
    serial command plane) so a wedged provision fails the tool call in-band.
  - **Despawn**: `store.AgentOwner(target)`; owner mismatch / unknown /
    non-agent → in-band `not_found`; target == caller → in-band
    `invalid_argument` (no self-despawn); else stop the target's live
    session (the hub's `sessionForAccount`; absent session skips), then
    `store.PlacementForAgent(target)` to resolve the container. A same-owner
    account with **no live placement** (`PlacementForAgent` → `ErrNotFound`)
    is an **already-torn-down idempotent success**: return
    `DespawnPeerResponse{}` without reaching `Hub.Remove`, mirroring
    `StopAgentSession`'s "stopping an already-stopped session succeeds"
    (`service.go:190-193`). Only an `AgentOwner` failure (unknown / other-owner
    / non-agent) is `not_found`; a placement-absent target never is. Otherwise
    `Hub.Remove`, `store.DeleteAgentPlacement`, unbind.
- `service.RemoveAgentWorkspace`: nil-hub → `CodeUnavailable`
  (`errNoRunnerHub` pattern, `service.go:111-113`); `hub.Remove`;
  `store.DeleteAgentPlacement` on success. Idempotent end to end.
- AdminGate: classify `RemoveAgentWorkspace` `adminOnly` (the `IssueToken`
  lane, `admin_gate.go:150-154`).

Interfaces:

```go
// go/server/lifecycle.go (new) — satisfies runnerhub.LifecycleCaller.
func (l *lifecycleService) SpawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) DespawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.DespawnPeerRequest) (*compassv1internal.DespawnPeerResponse, error)

// go/server/service.go
func (s *service) RemoveAgentWorkspace(ctx context.Context, req *connect.Request[compassv1.RemoveAgentWorkspaceRequest]) (*connect.Response[compassv1.RemoveAgentWorkspaceResponse], error)
```

Red-first: spawn inherits the CALLER's owner (assert the `CreateAgent` fake
received `callerOwner`, never the target's or admin's); a same-id spawn
retry joins and returns the identical response without a second Provision;
a mid-chain failure deletes the placement and a re-spawn of the same handle
succeeds; a same-handle spawn by a different owner → in-band
`already_exists`, never a resume; despawn by a different-owner caller →
in-band `not_found` identical to unknown-target; self-despawn refused;
despawn of a sibling the caller did not spawn but same-owner → succeeds;
a **second despawn of the same peer returns success, not `not_found`** (the
placement-absent idempotent path, exercising the Global-Constraints
invariant); `RemoveAgentWorkspace` handler test (removes + deletes placement; nil hub
Unavailable; unknown container succeeds); the admin-gate table test gains
the new procedure row (non-admin `PermissionDenied`).

### T6 — agent: gateway `Lifecycle` endpoint + the two tools

- Runner gateway: serve `AgentGateway.Lifecycle` beside `Comms` — resolve
  container→session (fail-closed `CodePermissionDenied` when no session is
  bound, the `Session` contract at `host.go:145-150`), forward
  `RelayLifecycleCall{session_id, call}` verbatim to the Server, return the
  result.
- Agent TS: extend the transport seam with `lifecycle(call)`; add
  `createLifecycleTools(broker)` returning `agents_spawn_peer` (`approval:
  "write"`; params `handle`, `display_name?`, `initial_prompt?`) and `agents_despawn_peer`
  (`approval: "write"`; params `agent_account_id`), registered at boot
  beside `createCommsTools` (`index.ts:13`). In-band `LifecycleCallError`
  renders as a thrown tool failure with the `(code): message` text shape the
  comms tools use (`comms.test.ts:487`).
  The broker derives `client_request_id` from the SDK `toolCallId` through
  the same nonce-scoped key the comms broker mints
  (`idempotencyKey(toolCallId)`, `comms.ts:93-96`, applied at
  `clientRequestId: broker.idempotencyKey(toolCallId)`, `comms.ts:286`) —
  pinning "one logical call" so the dedup key survives an agent-turn/model
  retry of the same tool call, the exact failure it exists for.

Interfaces:

```ts
// packages/compass-agent/src/lifecycle.ts (new, sibling to comms.ts)
export interface LifecycleTransport {
    lifecycle(call: LifecycleCallRequest): Promise<LifecycleCallResult>;
}
export class LifecycleBroker {
    constructor(transport: LifecycleTransport);
    call(call: LifecycleCallRequest): Promise<LifecycleCallResult>;
}
export function createLifecycleTools(broker: LifecycleBroker): AgentTool[];
```

Red-first: TS tool tests mirroring `comms.test.ts` (wire shape of a spawn
call incl. minted `client_request_id`; despawn param mapping; error
rendering); Go gateway test: a lifecycle call on an unbound socket
fail-closes `CodePermissionDenied`, a bound one forwards the exact
session id (the `e2e_transport_test.go:149-153` attribution proof, for the
new RPC).

### T7 — end-to-end red-first integration

The BDD spine, `integration_pgtest_test.go` pattern (socket → gateway →
relay → hub → `LifecycleCaller` → store): a bound supervisor session calls
spawn → a second agent account exists owned by the SUPERVISOR'S owner, a
container is
provisioned (fake runtime), a session starts, and the peer's own comms
call resolves to the peer's account; then despawn → container removed,
placement deleted, the peer's next call fails closed `CodeNotFound`; a
foreign-owner despawn attempt changes nothing.

Interfaces: none new — this task consumes T1-T6.

## Tasks

- [ ] T1 — proto delta (held-for-review): `Lifecycle` on `AgentGateway`,
  `RelayLifecycleCall` + `remove` oneof variants on `runner.proto`,
  `RemoveAgentWorkspace` on `compass.proto`; gen-fence + regen
- [ ] T2 — store: `DeleteAgentPlacement`, `PlacementForAgent`, `AgentOwner`,
  `AgentByHandle` (+ pgtest tests)
- [ ] T3 — runner: `agentHost.Remove` + `SessionsResponse_Remove` dispatch
  (+ tests)
- [ ] T4 — runnerhub: `Hub.Remove`, `Hub.RelayLifecycleCall` (identity
  resolution + `LifecycleCaller` delegation), `sessionForAccount`, handler
  mount (+ security tests)
- [ ] T5 — server: `LifecycleCaller` impl (spawn/despawn orchestration),
  `RemoveAgentWorkspace` handler + AdminGate `adminOnly` row (+ tests)
- [ ] T6 — agent: gateway `Lifecycle` endpoint, `LifecycleBroker`,
  `agents_spawn_peer` / `agents_despawn_peer` tools (+ tests)
- [ ] T7 — end-to-end pgtest integration: spawn→attributed peer→despawn→
  fail-closed

## Open Questions

All three load-bearing questions were resolved by Matt (2026-07-29); each
ruling and its deferred-work follow-up issue are recorded inline. The
top-level ownership model (F2) is ruled and NOT reopened here.

### OQ-1 — Can an agent despawn a same-owner sibling it did not spawn? (RESOLVED)

The F2 ruling gives the supervisor "spawn/despawn authority over its wave",
but the wave shares one owner — so any agent of that owner could despawn any
other, including a worker despawning the supervisor. Options were: (a)
same-owner scope, any agent may despawn any sibling; (b) spawner-only —
record a `spawned_by` column and require caller == spawner (plus the operator
lane). **Resolved (Matt, 2026-07-29): (a) same-owner for MVP** — "same owner
for MVP. we'll do auth scopes after we get up and running." No new schema;
the wave's supervisor topology is a convention the prompt enforces, not the
authz layer. Finer-grained caller-scoped authz (spawner-only, role-scoped, or
a capability model) is deferred to **RIG-1573**.

### OQ-2 — Initial prompt vs persona for the spawned peer (non-load-bearing — RESOLVED)

Reclassified non-load-bearing: its own recommendation leaves every task
unchanged (the record's definition), and it has since been resolved
externally. Persona is owned by compass-agent via RIG-1571, and Matt ruled
the persona SOURCE is a field on `AgentAccount` (a `persona TEXT` column +
`store.NewAgent`/`AgentAccount` field threaded through `CreateAgent`,
materialized to `$HOME/.compass/persona.txt` by the Runner, read into
`createAgentSession({systemPrompt: [persona]})` by the agent). **Resolved:**
spawn ships `initial_prompt` only — threaded to the existing
`StartAgentSessionRequest.initial_prompt` ("Optional initial prompt to send
once the session is ready", compass.proto:362-364) — while persona rides
the separate RIG-1571 AgentAccount-field seam; a `persona` field on
`SpawnPeerRequest` is an additive future add if spawn-time persona-set is
ever wanted.

### OQ-3 — Rate/quota limits on agent-driven spawn (RESOLVED)

A looping supervisor could provision containers unboundedly (each is a real
podman container + image build). Options were: (a) no limit for MVP
(single-owner dogfood, operator watches the board); (b) a per-owner
live-container cap enforced at the spawn handler (count live placements,
refuse above N with in-band `resource_exhausted`). **Resolved (Matt,
2026-07-29): (a) no limit for MVP** — "no limit for MVP, we can follow up
with a better limits design later (per user, per agent, etc)." Proper spawn
limits are deferred to **RIG-1574**. Consequence for OQ-6: with no cap
shipping, the **bounded relay deadline (Approach) is the sole MVP guard** on
how long an agent can monopolize the Runner's serial command plane.

### OQ-4 — Orphan or cascade when a spawner is despawned? (non-load-bearing for MVP)

Under same-owner ownership there is no parent edge at all — despawning the
supervisor leaves its spawned peers running, owned by the same human,
despawnable by any surviving sibling or the operator. Cascade would require
the `spawned_by` edge from OQ-1(b). **Recommendation: orphan (no cascade)** —
peers hold independent work-in-progress; killing them because their spawner
died destroys work, and the operator/supervisor-successor can despawn
explicitly. Non-load-bearing: nothing in the Plan depends on it; a cascade
could be added later atop a `spawned_by` column.

### OQ-5 — Should the operator-facing `RemoveAgentWorkspace` also delete the agent account? (non-load-bearing)

This record keeps accounts permanent (identity outlives compute). If Matt
wants a full "delete agent" operator lane (account + history), that is a
separate record — it touches comms history integrity, not the container
lifecycle. **Recommendation: no — keep despawn compute-only.**
Non-load-bearing: the Plan is unchanged either way.

### OQ-6 — Head-of-line blocking on the Runner's serial command plane (RESOLVED)

An agent-triggerable spawn holds the Runner's serial `Sessions` dispatch
loop for the full container build/pull: `RunSessions` handles one command
inline before receiving the next (`dispatch.go:104-112`), and Provision
runs minutes — so no Stop/Status/despawn/second-spawn can execute on that
Runner meanwhile. Today only operators trigger that; this record makes it
agent-triggerable in a loop. Options were: (a) accept head-of-line blocking
for the single-Runner MVP, guarded by the bounded relay deadline (Approach)
so a wedged provision fails the tool call instead of parking indefinitely;
(b) make Runner dispatch concurrent per command. **Resolved (Matt,
2026-07-29): (a) accept for MVP** — "accept for MVP, file follow up to design
fully later." The bounded relay deadline is the sole MVP guard (OQ-3 ships no
cap). Concurrent per-command dispatch — the router already correlates
out-of-order completions by request id (`router.go:33-36`), but concurrency
touches the dispatcher idempotency map and the send-serialization invariant
(`sendMu`, `router.go:38-40`) — is deferred to **RIG-1575**.
