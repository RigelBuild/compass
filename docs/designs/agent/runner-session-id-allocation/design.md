# Runner session-ID allocation: server-minted fresh IDs (RIG-3696)

Status: Active proposal

Issue: [RIG-3696](https://linear.app/rigelbuild/issue/RIG-3696) (P1, parent RIG-2861).
This record proposes a server-minted, restart-safe session-ID change. It is not an authoritative description of the currently shipped implementation. The current baseline at canonical implementation commit `a92fa2d0` remains Runner-local `monotonicIDs`, which emits process-local `sess-<counter>` values and resets after a Runner restart. The design is frozen on merge; later changes supersede it by citation.

## Problem and intent

A fresh `StartAgentSession` must not reuse an identifier after a Runner restart. The old Runner-local `monotonicIDs` counter restarted from zero in each process, while durable session and binding rows survived. A reused ID could make a new lifetime resolve to an old account. The security invariant is therefore:

> A `session_id` selects a Server-owned account binding. It never carries an account, and a fresh lifetime never reuses a prior lifetime's ID.

The planned target behavior, pending the implementation PR, makes the Server the authority for a fresh ID. The Runner receives that `fresh_session_id` on the internal `SessionsResponse` envelope, never mints a fallback, and echoes the selected ID in its start result. A resume reuses the already-authorized logical `resume_session_id`; it does not create a second logical session. The shipped baseline does not yet provide this server-minted envelope path.

## Decisions

### Authoritative binding and data flow

Planned target behavior (pending the implementation PR): `Hub.Start` is the sole fresh-start minting boundary. It sends `fresh_session_id` on the internal envelope, `dispatcher.execute` passes it to `agentHost.Start`, and the Runner returns the selected ID in its start result. The Server promotes the container's provisioned account binding only after that result. `promoteSession` records the live `session_id` to account binding and keeps its existing fail-closed behavior for unknown accounts. RelayCommsCall, RelayForgeCall, RelayBoardCall, RelayLifecycleCall, and FetchSecrets resolve the account from that binding; no relay trusts an account asserted by the Runner or client (`go/internal/runnerhub/relay_comms.go`, `relay_forge.go`, `relay_board.go`, `relay_lifecycle.go`, `handler.go`).

The planned internal field is a sibling of `request_id` and `resume_body`, outside the command oneof. It is not added to the public `StartAgentSessionRequest`. Fresh starts carry the Server-minted field. Resumes carry the existing `resume_body` and no fresh ID. The Runner selects `resume_session_id` first; otherwise it requires a non-empty `fresh_session_id` and uses it verbatim.

### Fresh start (planned target behavior; pending implementation PR)

1. `Hub.Start` mints a fresh ID and sends it as `fresh_session_id` with `Start`.
2. The Runner serializes the container transition, rejects an already-live container, and starts the agent with that ID.
3. The Server verifies the returned ID is the one it sent, then promotes the account binding and returns the response.
4. A returned-ID mismatch is version skew or a Server/Runner defect. The Server does not promote it. Before any cleanup Stop, it proves a container/attempt ownership fence: the reported ID must be bound to the same container and this start attempt. A reported ID that is foreign, unbound, or owned by another attempt is never passed to Stop. Only an ID that passes that fence may receive a bounded Stop, using a context independent of the cancelled start request. The Server returns an internal error naming both IDs; if fenced cleanup fails, the error log names the container and both IDs for operator cleanup.

A missing result variant is also a wire-contract error and fails without Stop: there is no returned ID to stop. A fresh start with no Server-minted `fresh_session_id` fails at the Runner with `FailedPrecondition`; it never falls back to a Runner-local counter.

### Resume (planned target behavior; pending implementation PR)

Resume authorization happens before the Runner call. The authorized logical ID is reused as the live ID. The Runner materializes the reconstructed session body before agent exec, validates the resume ID as a bare path element, and rejects malformed path input. A live collision for the logical ID returns `AlreadyRunning`. A fresh ID supplied alongside a resume is ignored in favor of the resume ID and logged as skew; production never sends both.

## ID construction and retry identity

Shipped baseline (`a92fa2d0`): the Runner-local `monotonicIDs` generator emits
`sess-1`, `sess-2`, and so on within one process. The counter resets after a
Runner restart, and the baseline does not provide deterministic retry identity
across restarts.

Planned target behavior (pending the implementation PR) follows DL-371 exactly.
The Server derives `fresh_session_id` from the **Server relay request ID** (the
normalized ID carried on the internal relay envelope), never from a client
value or Runner-local state. The input is domain-separated, length-prefixed,
and SHA-256 hashed:

```text
SHA256("compass.session-id.v1" ||
       u32be(len(relay_request_id)) || relay_request_id)
```

The digest is encoded as 64 lowercase hexadecimal characters. The domain label
and length prefix are frozen by DL-371. No container name, account ID, or
mutable retry metadata is included. A retry that reuses the same normalized
Server relay request ID therefore derives the same logical ID. A distinct
request ID derives a distinct ID. A request routed to another container MUST
use a fresh relay request ID, so the derivation cannot become a cross-container
selector. The Runner receives the derived value and never derives or
substitutes one.

The request ID is stable only for retries of one Server operation. Concurrent
generations use distinct relay request IDs and therefore distinct IDs; the
per-container transition lock remains the independent generation fence.

## Lifetime, generation, and locking fences

The logical session ID is stable across authorized resumes. A live lifetime is
still tied to the serving Runner **and exact container attempt** (Runner
enrollment identity plus container name). Runner enrollment clears the serving
Runner's live bindings; a stopped, unknown, or post-reconnect ID fails closed
rather than inheriting an old account. The Server's durable row remains the
authorization root, while the Hub's in-memory maps are the live dispatch cache.

Planned target behavior (pending the implementation PR): `agentHost.Start`
takes the per-container transition lock for the complete start, including slow
pre-exec work. Under the Runner mutex it checks for a live session on that
container before selecting the ID. This closes the check/start race that could
otherwise admit two generations. Resume selection also checks that the logical
ID is not already live. Stop, Remove, reload, and reconnect use the same
container/session transition discipline. Any mismatch cleanup must carry and
verify the container/attempt ownership fence before Stop; it must never act on
a foreign reported ID. A future multi-lifetime implementation must preserve
the invariant that old-lifetime frames cannot bind to a new lifetime; it must
use an explicit lifetime/generation fence rather than process uptime.

The durable transcript path (when enabled by the session-persistence design)
keeps sequence state scoped to the logical session and re-bases each Runner
lifetime. The session ID remains the stable key; a lifetime/generation marker
is not a client-selectable substitute for that key.

### Durable and cache lifecycle

| Event | Durable store (authoritative) | Hub/Runner cache (disposable) |
| --- | --- | --- |
| Enrollment | Keep authorized rows; mark the prior live attempt stale. | Clear every live binding for that Runner; accept only the new enrollment attempt. |
| Fresh start | Promote only after exact ID echo and attempt correlation. | Populate after promotion; never authorize from cache alone. |
| Reconnect | Keep logical session and transcript; do not rewrite ownership from reconnect traffic. | Drop old attempt bindings, then rebuild from authenticated durable state. |
| Crash/restart | Preserve logical session and transcript; create no new binding without promotion. | Discard pre-crash entries; stale IDs cannot route or stop a new attempt. |
| Stop/Remove | Release the matching durable live binding under the existing lifecycle operation. | Remove only the matching Runner/container-attempt entry. |

Cache loss is recoverable from the durable row. Durable-row absence or attempt
mismatch is fail-closed.

## Mismatch correlation and cleanup

A returned-ID mismatch is handled only after correlating the response to the
same authenticated Runner enrollment, container name, and container-attempt
token that performed the start. The Server does not stop by session ID alone.
If correlation is absent or differs, it records the mismatch and takes no
cleanup action. When correlation matches, cleanup may stop only that exact
attempt. It MUST NOT stop a foreign ID, a later generation, or an ID belonging
to another Runner/container pair. Cleanup uses a bounded context independent
of the cancelled start request. The Server never promotes the mismatched
result; if cleanup fails, the log names the correlated Runner, container,
attempt, and both IDs for operator cleanup.

This same rule applies to mixed-version responses: an old Runner's returned
local ID is never promoted, and is passed to Stop only when the authenticated
attempt fence proves it is the same live attempt.

The durable transcript path (when enabled by the session-persistence design)
keeps sequence state scoped to the logical session and re-bases each Runner
lifetime. The session ID remains the stable key; a lifetime/generation marker
is not a client-selectable substitute for that key.

## Fail-closed boundaries and resource bounds

The planned target behavior (pending the implementation PR) fails closed at every trust boundary:

- empty fresh IDs return `FailedPrecondition`; no Runner-local minting exists;
- a missing start result returns an internal wire-contract error without an invented ID;
- a Runner echo mismatch is never promoted;
- mismatch cleanup first proves container/attempt ownership, uses a bounded timeout and a context independent of the cancelled start request, and never Stops a foreign, unbound, or otherwise unowned reported ID;
- unknown or foreign session IDs resolve to no account;
- malformed resume IDs cannot escape the Runner's resume directory;
- resume bodies are materialized before exec, and a body without a resume ID is dropped with a warning rather than attached to a fresh session;
- `ErrConflict` in `promoteSession` remains a Server-bug witness, not a repair path.

The session-ID change adds no unbounded retry, archive, or body buffering. It does not broaden resume-body size or storage limits.

## Mixed-version compatibility and rollout ordering

The planned target behavior (pending the implementation PR) has no Runner version handshake. The planned rollout deploys the Server and Runner changes together, with the Server minting and echo validation enabled only once the deployed Runner accepts the internal field. A newer Runner with an older Server receives no `fresh_session_id` and rejects fresh starts with `FailedPrecondition`. An older Runner ignores the internal field and returns its old Runner-local ID; the newer Server detects the mismatch, performs fenced bounded cleanup only if the reported ID belongs to the same container and attempt, and returns `Internal` without recording a binding. Resumes remain compatible because the Runner gives precedence to the authorized `resume_session_id`. This is deliberate partial availability, not a compatibility shim; do not roll only one side and wait for fresh starts to recover.

## Alternatives rejected

- **Runner-local random or enrollment-prefixed IDs:** collision resistance does not change the authority problem; the Runner would still mint a Server-owned key.
- **Repair `ErrConflict`:** deleting or repointing durable ownership on an error path could steal an account and would hide the collision.
- **Public `fresh_session_id`:** a client-chosen storage key is an authorization bypass vector. The internal envelope is the only carrier.
- **Promote whatever the Runner returns:** this silently accepts old-version skew. Echo verification plus bounded, ownership-fenced cleanup makes skew loud and fail closed.
- **Per-call random IDs or request ID verbatim:** the former breaks retry idempotency; the latter conflates transient correlation identity with a durable public key.

## Acceptance criteria (planned target behavior; pending implementation PR)

1. A fresh start returns a non-empty server-minted `fresh_session_id`, and the
   Runner reports that exact ID; no Runner-local ID generator remains. The
   shipped baseline at `a92fa2d0` remains Runner-local `monotonicIDs` until
   this proposal lands.
2. The DL-371 derivation is explicit and tested: the same relay request ID
   gives the same ID, while distinct concurrent generations and distinct
   containers do not share one.
3. A resume returns its authorized logical ID, materializes its body before
   exec, and does not create or record a fresh ID.
4. Concurrent starts on one container cannot create two live generations; a
   resume cannot displace an existing live lifetime.
5. Missing, malformed, foreign, mismatched, or otherwise untrusted IDs fail
   closed. Mismatch cleanup is bounded, attempt-correlated, and never stops a
   foreign ID or records a binding.
6. All account-scoped relay paths resolve through the Server-owned binding, and
   reconnect/Stop/Remove release that binding so stale IDs cannot authorize.
7. Server and Runner mixed-version behavior is explicit: fresh starts fail
   precondition or internal mismatch; resumes remain usable; no silent fallback
   minting occurs.
8. Named regression commands cover the observable contract:
   - `go test ./internal/runnerhub -run '^TestStartRelayReturnsSessionIdOnSuccess$'` —
     fresh relay returns the echoed ID and promotes one binding.
   - `go test ./internal/runnerhub -run '^TestRelayCommsEveryArmAttributesToBoundAccount$'` —
     every relay arm executes under the durable binding, not request identity.
   - `go test ./internal/runnerhub -run '^TestFailClosedStoppedNeverSeenAndPostReconnect$'` —
     stopped, unknown, and post-reconnect IDs resolve to no account.
   - `go test ./internal/runnerhub -run '^TestEnrollFiresReapSinkWithClearedSessionIDs$'` —
     enrollment clears prior live IDs and reports the exact cleared set.
   - `go test ./internal/runnerhub -run '^TestConcurrentResolveDuringAFaultingReapCannotResurrect$'` —
     concurrent cache reap cannot resurrect a stale binding.
   - `go test ./internal/runner -run '^Test.*Start'` and
     `go test ./internal/runner -run '^Test.*Resume'` — start/resume cases
     assert preconditions, resume precedence, path guards, and the transition
     lock.

## Implementation references

- `go/internal/runnerhub/commands.go`: fresh-start relay, request identity, and promotion.
- `go/internal/runnerhub/relay_comms.go`: authoritative live binding and conflict behavior.
- `go/internal/runner/dispatch.go`: envelope-to-host threading and error map.
- `go/internal/runner/host.go`: per-container lock, ID selection, resume path, and pre-exec materialization.
- `go/internal/store/agent_sessions.go`: durable ownership and subscriber authorization.

The implementation changes Go and generated internal protocol surfaces; this record changes documentation only.
