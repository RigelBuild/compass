# Resume: bind the transcript base after the Runner accepts

> Freezes on merge; later changes supersede by citation, never rewrite.
> Linear: RIG-4297. Parent: `docs/designs/agent/compass-agent-session-persistence/design.md`
> § *T4* and § *T6*. Sequenced with RIG-3107. This record adds to the parent's
> rebase model; it does not rewrite it.

## Problem / Intent

Both resume paths bind the transcript base before the Runner accepts the
Start. `service.startResumeSession` (`go/server/service.go`) and
`lifecycleService.resumeSession` (`go/server/lifecycle.go`) call
`Store.BindLifetime` and only then call `Hub.StartResume`
(`go/internal/runnerhub/resume_start.go`). `BindLifetime`
(`go/internal/store/agent_transcripts.go`) sets
`agent_sessions.base_entry_seq` to the session's current `MAX(entry_seq)`.
`Store.AppendTranscriptEntry` reads that one base on every frame and writes
the row at `base + entry_seq`.

A frame does not say which lifetime sent it. `TranscriptEntry`
(`proto/compass/v1/agent.proto`) carries `entry_json`, `checkpoint`, and the
agent-stamped `entry_seq` (from 1 per agent process). The durable commit,
`Hub.CommitConversationFrame` (`go/internal/runnerhub/relay_comms.go`), is
keyed by the logical session id, and a resume reuses that id
(`agentHost.Start` in `go/internal/runner/host.go`).

So a refused resume still moves the base of a live session:

1. Lifetime L1 is live. It has written k frames at base B1.
2. A second resume calls `BindLifetime`. The base becomes B2 = B1 + k.
3. `agentHost.Start` refuses with `errAlreadyRunning`
   (`RUNNER_ERROR_CODE_ALREADY_RUNNING`, `go/internal/runner/dispatch.go`).
4. L1's next frame, k + 1, lands at B2 + k + 1. The rows B1 + k + 1 to
   B2 + k never exist.

`TestWakeAgentLiveElsewhereIsRefusedByRunner`
(`go/server/lifecycle_wake_pgtest_test.go`) witnesses the moved base. The gap
harms reads: `Hub.ReconstructSessionBody` (`go/internal/runnerhub/reconstruct.go`)
numbers each archived segment line as `MinEntrySeq + i`, so a segment that
spans a gap gets wrong seqs and merges out of order against the PG tail.

Today the window is narrow. There is one Server. The race needs a resume in
the gap between the Runner's Start result and the map write in
`Hub.promoteSession`, or a wake whose live check reads only the hub cache.
RIG-3107 (multi-instance) widens it: a Server whose cache does not hold a
session that another instance promoted will resume it.

Matt ruled Opt 3 + 1 on RIG-4297. Opt 3: accept the race now and document it.
Opt 1: design bind-after-accept, sequenced with RIG-3107. Frames stay without a
lifetime id. This record is the Opt 1 design for resume. The Opt 3 comments
ship as their own code PR.

`Reload` has a related bug that this record does not fix (see Open
Questions): `agentHost.reloadLocked` relaunches under the same session id with
no rebind and no resume file, so the new process's frame 1 hits
`ErrConflict` on the row the old process wrote.

## Approach

The Runner binds a resume's lifetime, not the Server. The bind moves to the one
point that already knows the Start is accepted and no frame of the new
lifetime exists yet: inside `agentHost.Start`, under `lockContainer`, after
the `errAlreadyRunning` checks and before `link.StartAgent`.

- New unary `RunnerService.BindLifetime(container_name, session_id)`, Runner to
  Server, on the internal `proto/compass/v1/runner.proto`. It returns an empty
  response. It is the same Runner-initiated unary shape as `FetchSecrets`. The
  Server still gains no inbound route.
- The handler authorizes like `Handler.FetchSecrets`: `Hub.AccountForContainer`
  for the authenticated Runner and the container. A resume already depends
  on that binding: `Hub.promoteSession` reads it after Start, and without it
  the resumed session gets no session binding and its comms calls fail
  closed. So the bind adds no new precondition. The Runner door sets no
  tenant, so the handler then resolves `Store.AccountTenant` under
  `store.WithSystemRole` and binds on
  `store.WithTenant(store.WithoutSystemRole(ctx), tenant)`, as
  `lifecycleService.wakeCtx` does. The bind matches only a session whose
  `agent_account_id` is that account. A foreign container, another account's
  session, and an unknown session all return the same `PermissionDenied`.
- A bind error fails the Start before the agent runs. The container lock is
  still held, so no second Start can interleave. The error maps to
  `RUNNER_ERROR_CODE_INTERNAL`, never `NOT_FOUND`: `resumeSession` treats
  `NOT_FOUND` as a missing container and would reprovision.
- A fresh Start skips the bind. `service.StartAgentSession` writes the
  session row only after Start returns, and the default base 0 is correct.
- `service.startResumeSession` and `lifecycleService.resumeSession` drop their
  `Store.BindLifetime` call. Authz, `ReconstructSessionBody`, `StartResume`,
  and the reprovision retry are unchanged. The retry binds once: the registry
  miss returns `errSessionUnknown` before the bind runs.

The invariant this relies on is at most one live lifetime per session, and
today single-Runner placement enforces it. The container lock serializes
binds across any number of Servers, because one Runner owns the container. It
does not serialize across Runners: a stale placement on Runner B can still
bind a session live on Runner A. Multi-Runner placement must close that before
it ships (non-load-bearing deferral below).

## Alternatives considered

### Bind on the Server after `StartResume` returns

The agent is already running when the result comes back, and its first frames
can commit before the bind. They land at the old base, on rows the previous
lifetime wrote. Closing that gap needs the Server to hold every commit for the
session until the bind, which is the same coordination with more moving parts.

### Restore the old base when the Runner refuses

L1's frames between the bind and the restore already landed at B2 + seq. A
compare-and-set restore cannot move those rows back.

### A lifetime id on every frame

The store could key the rebase per lifetime. It is a wire change on every
frame, and the ruling keeps frames without one. The parent record rejected it
for the same reason (§ *T4*).

### Derive the lifetime from the idempotency-key nonce

The agent mints each key as `<nonce>-<n>` with one nonce per process
(`createSocketFrameSink`, `packages/compass-agent/src/transport/frame-sink.ts`).
The Server could group by that prefix. It is a lifetime id in disguise, which
the ruling excludes, and it turns an opaque dedup key into a parsed field.

### Session-level advisory lock on the Server

It serializes Servers but still binds before the Runner decides. A refused
resume still moves the base.

## Global Constraints

- Frames carry no lifetime id. `TranscriptEntry` is unchanged.
- The new RPC is internal (`runner.proto`). No public proto changes.
- The bind runs under the session's tenant, resolved from the account. Never
  the door ctx, never the system role for the write.
- Fail closed: an unauthorized bind is `PermissionDenied` and matches an
  unknown session byte for byte.
- A bind failure never wraps `errSessionUnknown`.
- Tests advance on observed state (channels, `testing/synctest`), never sleeps.
- Land before RIG-3107 enables a second Server instance.

## Plan

1. **Move the resume bind to the Runner (one PR).** Proto and generated code,
   `Handler.BindLifetime`, `Hub.BindLifetime`, the account-scoped store bind,
   `ServerLink.BindLifetime`, and the call in `agentHost.Start`. The same
   commit removes the Server-side calls in `service.go` and `lifecycle.go`,
   so exactly one bind runs per lifetime, and it rewrites the Server pgtests
   that assert the Server-side bind. Split, the RPC would merge with no
   caller.

## Tasks

- [ ] **T1 — Runner binds a resume under the container lock** (`implement-go`).
  Interfaces:
  - `rpc BindLifetime(BindLifetimeRequest) returns (BindLifetimeResponse)`;
    request `{container_name string, session_id string}`, empty response.
  - `func (h *Handler) BindLifetime(ctx context.Context, req
    *connect.Request[compassv1internal.BindLifetimeRequest])
    (*connect.Response[compassv1internal.BindLifetimeResponse], error)`.
  - `func (h *Hub) BindLifetime(ctx context.Context, runnerID, containerName,
    sessionID string) error`: authz, tenant resolve, bind.
  - `Store.BindLifetime` becomes `func (s *Store) BindLifetime(ctx
    context.Context, sessionID string, account AccountID) error`; the sqlc
    `BindLifetime` query adds `AND agent_account_id = $2`. Missing or foreign
    row is `ErrNotFound`. Every caller and test moves in the same commit.
  - `func (l *ServerLink) BindLifetime(ctx context.Context, containerName,
    sessionID string) error`, called from `agentHost.Start` only when
    `req.GetResumeSessionId() != ""`.

  Tests:
  - Handler pgtest: a tenant-B session binds through the handler. A foreign
    container, an account mismatch, and an unknown session return identical
    `PermissionDenied`.
  - Host tests: a resume Start on a live container returns
    `errAlreadyRunning` and the fake `ServerLink` records zero binds. A bind
    error fails Start before `StartAgent` and maps to
    `RUNNER_ERROR_CODE_INTERNAL`.
  - Server pgtests: delete the `boundBase` assertions in
    `TestWakeAgentPriorSessionResumes`,
    `TestWakeAgentLiveElsewhereIsRefusedByRunner`
    (`go/server/lifecycle_wake_pgtest_test.go`), and
    `TestStartAgentSessionResumeKeyedOnStableLogicalIdAcrossResumes`
    (`go/server/service_resume_pgtest_test.go`). Their fake Runner never
    binds, so a base check there cannot fail. The refused-resume witness
    moves to the host test above.

  Comment sweep: the Opt 3 accepted-race comments in `service.go` and
  `lifecycle.go`, the bind-ordering comment on `Hub.StartResume`, the
  `BindLifetime` doc in `go/internal/store/agent_transcripts.go`, and the
  leg-five comment in `go/e2e/legfive_test.go`.

## Open Questions

- **Load-bearing for the Reload follow-up, not for this record: what is a
  Reload's transcript?** `reloadLocked` relaunches with `h.agentEnv(handle)`
  and no `ResumeSessionFile`, so the new process starts a new SDK session
  (`cli.ts` calls `manager.setSessionFile` only with a resume file). Today its
  frame 1 fails with `ErrConflict` and the tee fails the session. Rebinding
  alone would make two SDK sessions share one logical transcript. Options:
  (a) Reload is a resume: materialize a reconstructed body, then bind
  (recommended); (b) Reload mints a new logical session id; (c) Reload keeps
  the id and the new process opens with a checkpoint. Tracked on the
  human-action issue; a follow-up record designs the chosen option. The bind
  RPC above is the mechanism (a) and (c) would call.
- **Non-load-bearing deferral: in-flight commit across a rebind.** Under (a)
  or (c), the stopped process can have one `CommitConversationFrame` still in
  the Server after the Runner sees its call cancelled, because the Gateway
  commits on the agent's request ctx and the append reads the base and
  inserts in two statements. The follow-up must close it, either with a
  detached commit ctx plus a drain, or with row locks between bind and
  append. A resume needs neither: no frame of a live lifetime can be in
  flight when `Start` passes the live check.
- **Non-load-bearing deferral: cross-Runner fence.** Multi-Runner placement
  must refuse a bind when the durable `session_bindings` row names another
  Runner, before more than one Runner can hold an agent.
