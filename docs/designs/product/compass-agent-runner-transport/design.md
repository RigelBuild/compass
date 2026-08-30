# Compass agent↔Runner call transport

Status: Active

> Freezes on merge; later changes supersede by citation, never rewrite
> (`../compass-0.5/design.md:10-12`, convention restated in
> `../compass-0.6/design.md:1116-1118`). Tracked as RIG-1351.

## Problem / Intent

The first-party Compass agent, sealed inside its container, needs to make
correlated request/response calls to its Runner — comms (`PostMessage`, ask, etc.)
is the first caller, but the channel is general: it carries every future
agent-initiated call. The live stdio spine is telemetry-shaped — stdout is a
one-way newline-delimited `AgentFrame` stream relayed verbatim to `PublishEvents`
(`go/internal/runner/relay.go:138-172`) and stdin is a control lane
whose only current writer is a one-shot `sh -s` script feed
(`go/internal/runtime/agent.go:262-278`) — so overloading it with
request/response correlation would abuse a pipe built for fire-and-forget frames.
This record freezes a dedicated, Runner-sole call transport behind a seam, with a
Unix-socket-per-container concrete impl.

**Scope boundary (load-bearing).** This record designs the **agent→Runner
carrier** — the wire, the socket, the agent-side seam, and the Runner's
forwarding hop. The **Runner→Server leg** (the second hop of every call) is NOT
redesigned here: it is the already-designed `RelayCommsCall` of
`../compass-agent-comms-tools/design.md:369-382,577-642` — the Runner sends
`RelayCommsCallRequest{session_id, call}` and the **Server** resolves
`session_id → account` from its own Provision-originated binding and attributes
the call in-process under `comms.WithActor`, fail-closed. This record consumes
that leg (T3); it does not invent a new one. Why this split matters is spelled
out in Decision #3 and OQ-2.

## Approach

### Decisions (frozen by Matt — not reopened below)

1. **Runner-sole topology.** The agent NEVER contacts the Server directly. The
   Runner is the sole Server contact point; it forwards each agent-initiated call
   to the Server, which attributes it to the calling session's account. The
   Runner holds no account knowledge and asserts no account identity of its own
   (see Decision #3 and OQ-2 for how attribution actually happens).
2. **Off stdio.** The stdout `AgentFrame` telemetry spine and the (unbuilt) stdin
   control lane are NOT this channel. This transport is a separate, dedicated
   channel. stdio keeps its §T5 shape (`../compass-0.6/design.md:1410-1442`).
3. **One seam, `RunnerCallTransport`.** The agent↔Runner call transport is
   abstracted behind a single seam; comms is just its first caller.
   - Agent-side contract: emit a correlated call, await the result.
   - Runner-side contract: forward the call to the Server tagged with the
     `session_id` the Runner structurally owns (from which per-container socket
     the call arrived — see Decision #4), and return the result. The Runner is a
     pure forwarder: it resolves NO account and sets NO actor. **The Server**
     resolves `session_id → account` from its own authoritative,
     Provision-originated binding and sets `comms.WithActor` **in-process**
     (`../compass-agent-comms-tools/design.md:577-642`;
     `go/internal/comms/context.go:23-25`). Fail closed if no live
     session maps to the socket (Runner side) or the session does not resolve
     (Server side) — NEVER the bootstrap-admin fallback
     (`go/internal/comms/comms.go:330-334`).
   - The identity CONTRACT is transport-independent; only the agent→Runner
     carrier varies by impl. The account-attribution mechanism is Server-side and
     is the same regardless of carrier.
4. **Concrete impl now: Unix socket, bind-mounted per CONTAINER (1:1 with the
   session).** The Runner listens on a per-**container** Unix socket bind-mounted
   into the container; the agent speaks Connect/gRPC to the RUNNER over it
   (internal protos, local hop). **A container hosts exactly one agent for exactly
   one session over its whole life — container, agent, and session are 1:1, never
   reused across sessions.** So the socket IS that session's identity: the Runner
   binds socket → session statically, with no dynamic "current-session" remap and
   no between-sessions reuse. `podman run` (hence the bind-mount) happens once at
   Provision (`go/internal/runner/host.go:83-93` Provision→Launch;
   `:99-113` Start mints the session id), and the container is torn down when its
   session ends — socket, session, and container share ONE lifecycle. Wiring
   session-stop → container-teardown is the host-side change this rides on (today
   `Stop` leaves the container alive, `:146-156`; see Global Constraints). Zero
   network path ⇒ the nft egress posture is untouched, and the agent presents no
   token — there is nothing to leak or forge.
5. **Future network transport is an ADDITIVE impl of the same seam** — for the
   not-yet-narrowed hyperscaler topology (agents on EC2-class hosts, non-co-located
   with the Runner, where a Unix socket cannot reach). It drops in later
   (mTLS/token auth, an inherent egress path in that topology) WITHOUT reopening
   the contract. It is NOT designed now — no ground truth exists yet; this record
   only refuses to paint that corner shut.

### The seam

`RunnerCallTransport` is the agent-side seam: `call → Promise<result>`, one
pending entry per correlation id, correlation and deadlines owned by the
underlying Connect client. It mirrors the existing agent-side seams that hide
their wire envelope entirely — `FrameSink`
(`packages/compass-agent/src/frame.ts:50-52`) and `ControlSource`
(`packages/compass-agent/src/control.ts:58`). The comms-tools
record's `CommsBroker` (`../compass-agent-comms-tools/design.md:430-434`) becomes
a thin consumer that sits ON this seam: `broker.call()` delegates to
`transport.comms()`.

### Supersede-by-citation + comms-tools task disposition

The dependent comms-tools record recommends its transport fork as option (B),
"correlated request/response over the existing Runner **stdio** contract"
(`../compass-agent-comms-tools/design.md:707-719`). Matt's ruling #2 supersedes
that: the correlated call rides a dedicated socket transport, NOT stdio. Because
that record's task list is now a mix of live / dead / re-carried, this record
gives its explicit disposition so a reader executing comms-tools after this
freezes knows exactly which tasks stand:

| comms-tools task | Fate under this record | Why |
| --- | --- | --- |
| **T1** `comms_call` on `AgentFrame` + `comms_result` on `AgentControl` (`:313-363`) | **DIES** (the stdio-carrier variants) | The `AgentFrame.comms_call` variant and the `AgentControl.comms_result` first slice are the stdio carrier. The `CommsCallRequest`/`CommsCallResult`/`CommsCallError` **messages** survive but land HERE, in `agent_gateway.proto` (this record's T1) — they have not landed yet (comms-tools tasks all unchecked), so this is their first home, not a reuse. Landing the `AgentFrame`/`AgentControl` variants would add dead wire surface + a stray gen-fence symbol. |
| **T2** agent comms broker + first-slice stdin pump + native tools (`:416-527`) | **PARTIALLY SURVIVES** | Broker + native tools survive, rebased onto the seam (this record's T4). The **first-slice `ProtojsonLineSource` stdin pump DIES** — the Connect response is the comms result (OQ-3). |
| **T3** Runner intercept `comms_call` frame → relay → write result to stdin (`:528-575`) | **DIES** | The frame-divert + stdin-write dispatcher is replaced by this record's T2 (socket listener) + T3 (forward to Server). The *relay-to-Server intent* survives as this record's T3. |
| **T4** Server session→account resolution + hub `RelayCommsCall` handler (`:577-653`) | **SURVIVES VERBATIM — load-bearing** | This is the safe Runner→Server leg. This record's T3 forwards to exactly this handler. The Server-side resolution + `WithActor` in-process + fail-closed-on-missing-actor design is unchanged and is the reason this record needs no Runner-side account trust (Decision #3, OQ-2). |
| **T5** E2E wiring + freeze checklist (`:655+`) | **SUPERSEDED** by this record's T5 | stdio wiring → socket wiring. |

The `ProtojsonLineSource` stdin pump comms-tools proposed
(`../compass-agent-comms-tools/design.md:483-510`) is no longer the comms-result
path (OQ-3).

### The concrete impl (Unix socket per container)

- The Runner opens a per-**container** Unix socket on the host, `net.Listen("unix",
  path)`, mode 0600, and serves an `AgentGateway` Connect handler over it with
  cleartext HTTP/2 — the exact shape the Server's socket door already uses
  (`go/server/serve.go:219-223`: `&http.Server{Handler: mux,
  Protocols: cleartextHTTP2()}`).
- **Socket, session, and container share ONE lifecycle (1:1).** The socket is
  created and its listener started at **Provision, before `podman run`** (so the
  bind-mount source exists when the container launches —
  `go/internal/runner/host.go:83-93`), and torn down when the session
  ends and the container with it. Because a container hosts exactly one session
  for its whole life (Decision #4), there is no recurring "between sessions"
  window and no next session to strand — the socket→session binding is fixed at
  birth. The handler still fails closed (`CodePermissionDenied`) until that one
  session is bound (a call arriving after Provision but before Start) and once it
  ends.
- **Stale-socket recovery.** A Runner crash/restart leaves the socket file on
  disk; the next `net.Listen("unix", path)` then fails `EADDRINUSE`.
  `listenAgentSocket` reclaims the path only after an `Lstat` confirms a socket
  owned by the Runner uid, then removes it before listening; any other object
  (regular file, dir, symlink, or wrong owner — a path collision or partial op,
  never an abandoned Runner socket) is rejected fail-closed, not deleted. Pinned
  by a T2 test.
- The socket file is bind-mounted into the container via the existing `mountArg`
  path, which appends SELinux `:Z` unconditionally
  (`go/internal/runtime/podman.go:605-609`). The `:Z` relabel targets
  a socket inode; the T2 test on an enforcing host verifies the agent can still
  `connect()` after relabel.
- The agent dials that in-container socket path with a Connect/gRPC client over
  the local hop, speaking the internal `AgentGateway` protos.
- **Why egress stays sealed:** the hop is a Unix socket, not a network address.
  There is no new port and no outbound route; the nft posture
  (`../compass-agent-container-runtime.md:206-217`, MVP allow-all today, the
  future default-deny opt-in untouched) is neither relied on nor disturbed.
- **Why identity is structural, no credential:** one socket per container means
  the Runner already knows which container the connection belongs to, and maps it
  to the one session bound to that container (1:1). The account is never resolved on the
  Runner — the Runner forwards the `session_id` to the Server, which resolves the
  account from its own binding. The agent presents no token; there is nothing to
  leak, steal, or forge from inside the container.
- **Host socket-directory perms.** 0600 on the socket file is only meaningful if
  its parent directory is not traversable by other host users. The per-container
  socket lives under a Runner-owned dir created 0700; the T2 test pins the dir
  mode.
- **Cost paid to the container runtime:** rootless-podman uid mapping so the
  non-root agent user can open the mounted socket — the Runner-created socket is
  owned by the host Runner uid, and `--userns=keep-id:uid=<agent-uid>,
  gid=<agent-gid>` (`../compass-agent-container-runtime.md:634-638`) maps that uid
  to the baked agent uid in-container, so the agent owns the socket it must open.

### Alternatives considered

- **Direct agent→Server (a `CommsService` client in the container).** Rejected:
  it punches an egress hole in the sealed container, forces an account-scoped
  credential to live inside the agent (a steal/leak target), and reverses the
  `@connectrpc` fence that deliberately keeps daemon stubs out of the agent
  (`biome.json` `noRestrictedImports`). Three real costs, structural,
  not a "moat" argument.
- **Runner asserts the resolved account to the Server (Runner-side resolution).**
  Rejected as a security hole — see OQ-2. There is no Server-side mechanism for a
  Runner to speak for an account (`comms.WithActor` is set only by the
  account-bearer interceptor, `go/internal/auth/interceptor.go:35-38`;
  the Runner door sets only a `SubjectRunner` subject,
  `go/internal/runnerhub/auth.go:79-86`; the actor key is unexported
  precisely so identity can't be spoofed through a request field,
  `go/internal/comms/context.go:12-17`). Building one would grant an
  enrolled Runner the power to execute comms as ANY account it names. The safe
  path (Server resolves from its own binding) is already designed
  (`../compass-agent-comms-tools/design.md:577-642`) and is what Decision #3 / T3
  use instead.
- **stdio-B (correlated request/response over the stdout/stdin spine).**
  Rejected: overloads a telemetry pipe — stdout is a one-way Runner-sequenced
  frame relay (`go/internal/runner/relay.go:138-167`) — with
  request/response correlation and deadlines it was never shaped for, and forces
  the mid-turn consumption workaround the comms-tools record had to invent
  (`../compass-agent-comms-tools/design.md:483-510`).
- **Open one network port now (pre-build the future transport).** Rejected: it
  pays a standing egress carve and an in-container credential today, before the
  hyperscaler topology that would justify them even exists. The seam lets that
  impl land additively when its ground truth arrives (Decision #5).
- **Hand-rolled 4th fd / extra pipe.** Rejected: `podman exec` hands the child
  exactly three std pipes — `StdinPipe`/`StdoutPipe`/`StderrPipe`
  (`go/internal/runtime/podman.go:438-448`), with the struct fixed at
  three (`StreamingIO`, `go/internal/runtime/podman.go:179-183`) —
  so there is no clean fourth-fd hand-off, and a raw pipe re-implements the
  correlation, framing, deadlines, and cancellation that Connect/gRPC gives for
  free.

## Global Constraints

- **Egress seal preserved.** The transport is a local Unix socket with no network
  path; it neither relies on nor perturbs the nft posture
  (`../compass-agent-container-runtime.md:206-217`).
- **RIG-1267 gen-fence: internal protos only + extend the symbol list.** The
  `AgentGateway` service and its messages are INTERNAL — new declarations in the
  owned `compass.v1` package, generated ONLY into the internal lanes
  (`go/internal/gen` via `buf.gen.internal-go.yaml`; `compass-agent/src/gen` via
  `buf.gen.agent-ts.yaml`) and excluded from the public trees by `buf.gen.yaml`
  `exclude_paths`. **The gen-fence grep is a FIXED literal symbol list**
  (`proto/moon.yml:123`:
  `AgentFrame|AgentControl|SessionFrame|RunnerService|RunnerError|compassv1internal`)
  with an explicit maintenance instruction "Extend the symbol list as internal
  messages are added" (`:118-119`). A leaked `AgentGateway`/`CommsCall*` symbol
  matches NONE of the current terms (the TS public tree emits no `compassv1internal`
  string), so the fence would pass green on a real leak. **T1 MUST extend the grep
  list with `AgentGateway` and `CommsCall`** (the latter also covers the
  `CommsCallRequest`/`CommsCallResult`/`CommsCallError` messages this record
  lands). Content-scoped caution (same as comms-tools T1,
  `../compass-agent-comms-tools/design.md:385-396`): the fence is `grep -rlE` over
  file *content*, so a public message's generated doc comment must not quote an
  internal `AgentGateway`/`CommsCall*` name.
- **`@connectrpc` biome fence needs a NARROW carve AND a widened ban.** The fence
  is a Biome `style/noRestrictedImports` rule (level error) in
  `biome.json` (`root: false`, so it governs all of ``
  including the agent package). It bans `@connectrpc/connect` and
  `@connectrpc/connect-web` via a `paths` map plus a `patterns` glob group
  (`biome.json:13-25`), with one existing carve: an `overrides` entry setting the
  rule `off` for `packages/compass-client/**` (`biome.json:38-46`). Note the fence
  does **NOT** currently ban `@connectrpc/connect-node` — the Node transport this
  record adds. The carve is therefore three-part and file-scoped:
  1. **Widen the ban:** add `@connectrpc/connect-node` (+ its `/**` pattern) to
     the `paths`/`patterns` groups, so the transport override is the ONLY door to
     it — otherwise any agent module could import connect-node freely once it is a
     dependency.
  2. **Add deps:** add `@connectrpc/connect` + `@connectrpc/connect-node` to the
     agent package's `dependencies` (`packages/compass-agent/package.json`, which
     depends on no `@connectrpc/*` today), mirroring how the carved compass-client
     package holds Connect.
  3. **Scope the carve:** add ONE `overrides` entry scoping `noRestrictedImports`
     `off` for the single new transport module path
     (`packages/compass-agent/src/transport/**`), NOT the whole package.
- **Transport deadline default.** `transport.comms()` carries a default deadline
  of 30s (matching the comms-tools broker default,
  `../compass-agent-comms-tools/design.md:430-434`), enforced by the Connect
  client on the agent side; the Runner-side forward propagates the inbound
  context deadline to the `RelayCommsCall`. Named here so it is chosen once, not
  left implicit.
- **Rootless-podman uid map + 1:1 socket/session/container lifecycle.** Depends on
  the `--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>` mapping
  (`../compass-agent-container-runtime.md:634-638`) so the mapped in-container
  agent uid owns the Runner-created socket. **Rides on a host-side lifecycle
  change:** the 1:1 model (Decision #4) requires session-stop to tear the
  container down, but today `Stop` deletes the session and leaves the container
  running (`go/internal/runner/host.go:146-156`), and the
  `AgentRuntime.Teardown` primitive that stops+removes the container already
  exists but has NO lifecycle caller
  (`go/internal/runtime/agent.go:190-203`). Wiring session-stop →
  `Teardown` (and creating the socket at Provision / removing it at that teardown,
  plus stale-socket recovery) is a change to the runner/runtime + lifecycle record
  this record consumes, not owned here.
- **Red→green testing.** Every task lands its BDD + unit tests first (watch them
  fail), then the smallest impl to green (`rule://red-green-testing`).
- **Format / lint gates.** Run per-area only, via
  `direnv exec . moon run <project>:<task>` (biome, gofmt, golangci, buf
  lint/breaking, markdownlint). MD013 is disabled repo-wide
  (`.markdownlint.json` `{"MD013": false}`, repo root; `compass` inherits it).
- **Frozen-record convention.** This record freezes on merge; it supersedes the
  comms-tools OQ-1 transport recommendation and the §T5 framing that stdio is the
  agent's sole structured channel BY CITATION, never by rewriting those records
  (disposition table above).
- **Internal protos additive + buf-breaking-safe.** `AgentGateway` is a new
  service in the existing `compass.v1` package — a pure addition; `buf lint` /
  `buf breaking` still glob `compass/**/*.proto` and cover it. A new internal
  proto file also needs M-mappings for its imported public types added to BOTH
  plugins in `buf.gen.internal-go.yaml` (T1).

## Plan

Dependency-ordered. T1 defines the wire; T2 (Runner listener) and T4 (agent
client) both depend on T1 and can proceed in parallel; T3 (Runner→Server forward)
depends on T2 and on comms-tools T4 (the Server handler it calls); T5 wires
end-to-end and is last.

1. **T1 — Transport wire: the `AgentGateway` internal proto service + gen-fence extension.**
2. **T2 — Runner-side socket listener + per-CONTAINER socket lifecycle + uid map.**
3. **T3 — Runner→Server forward: socket→container→session, RelayCommsCall (fail closed).**
4. **T4 — Agent-side `RunnerCallTransport` client behind the seam + biome carve.**
5. **T5 — End-to-end wiring + live-turn E2E.**

## Tasks

### T1 — Transport wire: the `AgentGateway` internal proto service + gen-fence extension

Define the agent→Runner call service. This is the Runner-side LISTENER for agent
calls — it is NOT `RunnerService`, which is Runner→Server, dial-out
(`proto/compass/v1/runner.proto:17-58`). `AgentGateway` runs the
opposite direction: the agent is the client, the Runner is the server. First and
only RPC this slice: `Comms`. The `CommsCallRequest`/`CommsCallResult`/
`CommsCallError` messages are defined HERE (they have not landed yet — comms-tools
is unmerged), in a new internal file, not reused from an existing tree.

`Interfaces:` (internal proto, `compass.v1`, routed to the internal lanes only —
new file `proto/compass/v1/agent_gateway.proto`, OQ-4)

```proto
// agent -> Runner, unary. The agent emits a correlated comms call; the Runner
// forwards it to the Server (RelayCommsCall) tagged with the session it
// structurally owns. INTERNAL surface (never public gen).
service AgentGateway {
  rpc Comms(CommsCallRequest) returns (CommsCallResult);
}

message CommsCallRequest {
  string call_id = 1;              // agent-minted correlation id (SDK toolCallId)
  oneof call {
    PostMessageRequest post = 2;
    ListMessagesRequest list = 3;
  }
}
message CommsCallResult {
  string call_id = 1;
  oneof result {
    PostMessageResponse post = 2;
    ListMessagesResponse list = 3;
    CommsCallError error = 4;      // in-band failure: tool error, not stream teardown
  }
}
message CommsCallError { string code = 1; string message = 2; }
```

(`call_id`/`client_request_id` set from the tool-call id per
`../compass-agent-comms-tools/design.md:430-434`. The message shapes match that
record's T1 verbatim so the Runner→Server `RelayCommsCallRequest.call` field —
which is a `CommsCallRequest` — takes them unchanged.)

Generated handler/client interfaces (buf output, internal lanes):
`compassv1internalconnect.AgentGatewayHandler` (Go, Runner side),
`AgentGateway` service client (TS, agent side).

`Red→green:` add the proto, wire it into `buf.gen.internal-go.yaml` (including M-
mappings for its imported public types on BOTH plugins) + `buf.gen.agent-ts.yaml`
paths (NOT `buf.gen.yaml`), and **extend the gen-fence grep list with
`AgentGateway` and `CommsCall`** (`proto/moon.yml:123`). RED: with the
symbol added to the grep but `exclude_paths` NOT yet set, regenerate — the leaked
`AgentGateway` symbol lands in a public tree and the gen-fence greps RED (this is
the test that the fence actually bites; without the grep extension it would pass
green on the leak). `buf breaking` proves the addition is non-breaking. GREEN: add
the `exclude_paths` entries; internal gen lanes emit the handler/client; public
trees stay clean; gen-fence + `buf lint`/`buf breaking` pass.

### T2 — Runner-side socket listener + per-CONTAINER socket lifecycle + uid map

A new Runner component that, per **container**, opens a 0600 Unix socket on the
host (under a 0700 Runner-owned dir), serves the `AgentGateway` handler over
cleartext HTTP/2, bind-mounts the socket into the container, and tears it down at
container teardown. Mirror the Server socket-door serve shape
(`go/server/serve.go:219-223`). The socket is created BEFORE `podman
run` (at Provision, `go/internal/runner/host.go:83-93`) so the
bind-mount source exists; removed at container teardown, which the 1:1 model ties
to session stop (the host-side lifecycle change this record rides on).

`Interfaces:` (new package `go/internal/runner/gateway`)

```go
// Per-CONTAINER Unix-socket Connect server for agent -> Runner calls.
type SocketListener struct { /* path string; srv *http.Server; ln net.Listener */ }

// listenAgentSocket reclaims a stale path only if an Lstat confirms a
// Runner-owned socket (else fails closed), creates path (0600) under a 0700
// dir, net.Listen("unix", path), and serves h over cleartext HTTP/2. Called at
// Provision, before podman run.
func listenAgentSocket(ctx context.Context, path string, h http.Handler) (*SocketListener, error)

// Close: http.Server.Shutdown under a bounded deadline (drain in-flight), then
// srv.Close() to force any handler still blocked past it — the forced close is
// what delivers the promised Connect error to a broker blocked in
// RelayCommsCall — then os.Remove(path). Called at container teardown.
func (l *SocketListener) Close(ctx context.Context) error

// Mount describes the socket bind-mount handed to the runtime (mountArg path).
func (l *SocketListener) Mount(containerPath string) runtime.Mount
```

`Red→green:` RED: a unit test dialing the socket with a Connect client over a
Unix transport before `listenAgentSocket` exists (connection refused); a
lifecycle test asserting the socket file is absent before create, present + mode
0600 (in a 0700 dir) after, and gone after `Close()`; a stale-socket test
asserting `listenAgentSocket` reclaims a leftover *socket* at the path but
rejects fail-closed a leftover non-socket or wrong-owner object (regular file,
dir, or symlink — never deleted). A uid-map test asserting the mounted socket is
owned by the mapped agent
uid in-container and is `connect()`-able after the `:Z` relabel (extends the
container-runtime T8 agent-owns invariant, on an enforcing host). GREEN: listener
serves a stub `AgentGateway` returning `Unimplemented`; lifecycle + perms +
stale-recovery + uid pass. Non-goal this task: the Server forward (T3).

### T3 — Runner→Server forward: socket→container→session, RelayCommsCall (fail closed)

The `AgentGateway.Comms` handler maps the arriving connection → the container it
belongs to → the one session bound to that container (1:1, fixed at Start,
immutable thereafter — no dynamic "current-session" remap), then forwards the call
to the Server as `RelayCommsCall(session_id, call)`
(`../compass-agent-comms-tools/design.md:369-382`). **The Runner resolves no
account and sets no actor** — the Server resolves `session_id → account` from its
own binding and attributes in-process under `WithActor`, fail-closed
(`../compass-agent-comms-tools/design.md:577-642`). The Runner already holds the
container→session mapping: `agentHost.sessions` is keyed by session id with a
`containerName` field, and Start already scans it for a container match
(`go/internal/runner/host.go:106-113,131-139`).

`Interfaces:`

```go
// Resolves the one session bound to the container (1:1, fixed at Start) from
// the per-container socket the call arrived on. No account anywhere on the Runner.
type SessionForContainer interface {
    Session(containerName string) (sessionID string, ok bool)
}

// Comms implements compassv1internalconnect.AgentGatewayHandler by forwarding to
// the Server's RelayCommsCall under the resolved session. Fails closed
// (CodePermissionDenied) when no live session maps to the container.
func (g *Gateway) Comms(
    ctx context.Context, req *connect.Request[compassv1internal.CommsCallRequest],
) (*connect.Response[compassv1internal.CommsCallResult], error)
```

The forward propagates the inbound deadline to `RelayCommsCall`. A Server-side
`CommsCallError` (in-band tool failure) is returned as the `error` variant of
`CommsCallResult`, NOT a Connect stream error; a genuine transport failure
(Server unreachable mid-call) surfaces as a Connect error the agent renders as an
in-band tool error too (the agent side never tears the turn down on it, N/OQ-6).

`Red→green:` RED: a handler test where the container has NO session bound yet
(socket live at Provision, before Start mints the session), asserting a
fail-closed `CodePermissionDenied` (NOT a bootstrap-admin-attributed
side effect, NOT a forward with an empty session id); a happy-path test asserting
a container with its bound session forwards `RelayCommsCall{session_id, call}` to a
fake Server and returns its result. GREEN: the container→session lookup +
forward; all pass. Account attribution correctness is proven Server-side by
comms-tools T4's tests (author = agent account), which this task depends on.

### T4 — Agent-side `RunnerCallTransport` client behind the seam + biome carve

A new agent module implementing `RunnerCallTransport` over a Connect client that
dials the in-container socket path with `@connectrpc/connect-node`. connect-node
reaches a Unix socket via its HTTP/2 transport's `nodeOptions`
(`{ socketPath }` on the connection) with a placeholder `baseUrl` — named here so
the impl doesn't discover it late. The `CommsBroker`
(`../compass-agent-comms-tools/design.md:430-434`) is rebased onto it:
`broker.call()` delegates to `transport.comms()`; the broker keeps its
correlation bookkeeping but its `resolve()` side-channel is no longer fed by a
stdin pump (the Connect response is the result — OQ-3).

`Interfaces:` (new `packages/compass-agent/src/transport/`)

```ts
export interface RunnerCallTransport {
  comms(req: CommsCallRequest): Promise<CommsCallResult>;
}

// Concrete impl: Connect client over the bind-mounted Unix socket.
export function createUnixSocketTransport(socketPath: string): RunnerCallTransport;
```

Biome carve (three-part, per Global Constraints): (1) widen the ban with
`@connectrpc/connect-node`; (2) add `@connectrpc/connect` +
`@connectrpc/connect-node` to `packages/compass-agent/package.json`
`dependencies`; (3) add ONE `overrides` entry in `biome.json` setting
`style.noRestrictedImports` `off` for `packages/compass-agent/src/transport/**`
only (mirroring the existing `packages/compass-client/**` override,
`biome.json:38-46`).

`Red→green:` RED: a `biome check` run failing on the new transport module's
`@connectrpc` import BEFORE the override lands (the module also imports
`@connectrpc/connect`, so the fence bites; proves the carve is real, not vacuous);
a transport unit test issuing a `comms()` call against a Unix-socket test server
and asserting the awaited result. GREEN: add the widened ban + scoped override +
deps; `biome check` passes with the fence still active for every other agent
module (including connect-node); the transport test greens.

### T5 — End-to-end wiring + live-turn E2E

Wire the Runner to create + mount + serve the per-container socket at Provision
and tear it down at container teardown; wire the agent entrypoint to construct the
transport from the known in-container socket path (passed via env, OQ-5) and hand
it to `createCommsTools` via the broker. Prove a comms tool call flows agent →
socket → Runner → Server → back, mid-turn, attributed to the session's account.

`Interfaces:` spawn-path wiring in the runtime/runner Provision sequence
(`go/internal/runtime/agent.go`, `go/internal/runner/host.go:83-93`)
to allocate the socket before `podman run` and add its `runtime.Mount`; agent-side
construction in the container entrypoint that builds
`createUnixSocketTransport(socketPath)` and passes the broker to
`createCommsTools` (`../compass-agent-comms-tools/design.md:436-438`).

`Red→green:` RED: an E2E driving a real comms tool call DURING a live agent turn
(the turn is blocked on `broker.call()` while the result must arrive) — this is
the scenario the comms-tools OQ-8 flagged as a deadlock over stdio
(`../compass-agent-comms-tools/design.md:792-801`). Over the socket it must green:
the deadlock was a pull-cadence problem — a `comms_result` routed through the
agent's single control-loop iterator could not be consumed by that same loop while
it was suspended awaiting the turn. The Connect unary response is delivered by the
Node event loop WITHOUT any `ControlSource` pull, so the await resolves without
re-entering the suspended loop. (This is the invariant T5 asserts; it is not a
separate OS thread — Node is single-event-loop — but the delivery path does not
route through the control iterator.) In-flight-at-teardown: `Close()` runs
`http.Server.Shutdown` under a bounded deadline to drain a live call, then
`srv.Close()` force-closes any handler still blocked past it (`Shutdown` waits
but never force-closes, so the force step is what guarantees delivery); a call
still outstanding when the session dies surfaces to the blocked `broker.call()`
as a Connect error rendered in-band (acceptable — the session is going away).
GREEN: full wiring; the
live-turn call returns the Server's result attributed to the session's account;
the socket is cleaned up at container teardown.

## Open Questions

Load-bearing residual forks, each with a recommendation, PARKED for Matt's
morning ruling (overnight posture — designed against the recommended assumption,
not blocked on `ask`). The five Decisions above are frozen and are NOT reopened
here.

- **OQ-2 (LOAD-BEARING, security) — Confirm the Runner→Server attribution model.**
  This record's T3 uses the **Server-side resolution** model: the Runner forwards
  `RelayCommsCall{session_id, call}` and the Server resolves `session_id → account`
  from its own Provision-originated binding and sets `WithActor` in-process,
  fail-closed (`../compass-agent-comms-tools/design.md:577-642`;
  `proto/compass/v1/compass.proto:221-225` — the Server ORIGINATES
  `agent_account_id`). An earlier draft of this record instead had the **Runner
  assert the resolved account** to the Server as a trusted actor — that is
  **rejected as unsafe**: no Server-side mechanism exists for it (`WithActor` is
  set only by the account-bearer interceptor,
  `go/internal/auth/interceptor.go:35-38`; the Runner door sets only a
  `SubjectRunner` subject, `go/internal/runnerhub/auth.go:79-86`; the
  actor key is unexported anti-spoof, `go/internal/comms/context.go:12-17`),
  and building one would let an enrolled Runner execute comms as ANY account it
  names. *Recommendation:* the Server-side model (comms-tools T4), which needs no
  new trust mechanism, keeps the Runner account-free, and is already fail-closed.
  **Matt: confirm this is the intended trust model** (the alternative is the
  rejected Runner-asserts-account path; there is no third safe option today).
- **OQ-3 — Confirm the stdin pump is dropped from comms-tools scope.** The socket
  transport dissolves the comms-tools OQ-8 mid-turn deadlock (see T5), so the
  `ProtojsonLineSource` stdin pump (`../compass-agent-comms-tools/design.md:483-510`)
  is no longer the comms-result path. *Recommendation:* drop it from comms-tools
  scope (it may still be wanted later for other control, but not for comms
  results); make T5's live-turn E2E the proof, not a `resolve()` shortcut.
- **OQ-4 — New proto file vs additive RPCs on an existing internal service?**
  `AgentGateway` CANNOT be added to `RunnerService` (opposite direction, frozen
  three-RPC shape, `proto/compass/v1/runner.proto:17-58`).
  *Recommendation:* a NEW internal file `proto/compass/v1/agent_gateway.proto` in
  the `compass.v1` package, routed through the two internal gen lanes and excluded
  from `buf.gen.yaml` — keeps the agent→Runner direction its own reviewable
  surface. (Requires the M-mapping + gen-fence steps in T1.)
- **OQ-5 — Socket path + mount convention.** *Recommendation:* a per-**container**
  host path under the Runner's runtime dir (e.g. `<runtime>/containers/<container>/agent.sock`,
  container-keyed per Decision #4 — NOT session-keyed), bind-mounted to a fixed
  in-container path (e.g. `/run/compass/agent.sock`) passed to the agent via env,
  so the agent needs no per-session configuration. Non-load-bearing detail —
  deferrable to implementation, flagged so the convention is chosen once.
- **OQ-6 — Agent-side rendering of a transport failure.** comms-tools defined an
  in-band `CommsCallError` for a transport failure
  (`../compass-agent-comms-tools/design.md:700-701`); the agent must render a
  Connect-level failure (Server unreachable mid-call) as an in-band tool error,
  never a turn teardown. *Recommendation:* map any Connect error from
  `transport.comms()` to a synthetic `CommsCallError{code,message}` at the broker
  boundary, so the model sees a tool error and the turn survives. Non-load-bearing;
  recorded so the failure path is deliberate.
- **OQ-7 — Concurrency/backpressure: max in-flight calls per agent.** comms-tools
  T3 assumed a per-call goroutine on the Runner side; this record's Runner handler
  is a Connect unary handler (one goroutine per call, HTTP/2-multiplexed) so
  concurrent calls from one agent fan out naturally. *Recommendation:* no explicit
  cap for the MVP (the agent's own turn loop issues calls roughly serially); revisit
  if a future caller batches. Non-load-bearing; recorded so the absence is
  deliberate.
- **OQ-8 — Should the outbound telemetry spine eventually migrate onto this
  transport?** Today stdout→`AgentFrame`→`PublishEvents` is a separate
  Runner-sequenced relay (`go/internal/runner/relay.go:138-167`).
  *Recommendation:* leave it on stdout — it is a one-way high-volume stream whose
  verbatim-relay + gap-detection properties are well served by the pipe; folding
  it into a request/response transport buys nothing. Non-load-bearing; recorded so
  the boundary is deliberate, not accidental.

*RIG-1351.*
