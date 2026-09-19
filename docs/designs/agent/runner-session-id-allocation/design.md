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

Planned target behavior (pending the implementation PR): `Hub.Start` is the sole fresh-start minting boundary. It creates the Server-owned relay operation ID, derives `fresh_session_id`, and sends both on the internal `SessionsResponse` envelope. `dispatcher.execute` passes the envelope fields to `agentHost.Start`; the Runner returns the selected ID and authenticated attempt metadata in its start result. The Server promotes the container's provisioned account binding only after exact echo and attempt validation. `promoteSession` records the live `session_id` to account binding and keeps its existing fail-closed behavior for unknown accounts. RelayCommsCall, RelayForgeCall, RelayBoardCall, RelayLifecycleCall, and FetchSecrets resolve the account from that binding; no relay trusts an account asserted by the Runner or client (`go/internal/runnerhub/relay_comms.go`, `relay_forge.go`, `relay_board.go`, `relay_lifecycle.go`, `commands.go`).

The internal `fresh_session_id` is not the public `StartAgentSessionRequest` input and is never caller-selectable. The public `session_id` is the logical, durable key returned after promotion and used by later account-scoped calls. The internal `fresh_session_id` is only the Server-to-Runner value for a new lifetime; it is not a second public identifier and is not accepted from clients. The internal envelope also carries the Server-owned operation ID and exact container-attempt token needed for correlation. A caller-supplied request ID, fresh ID, account ID, or attempt token is rejected before minting.

### Fresh start (planned target behavior; pending implementation PR)

1. `Hub.Start` validates that the public request contains no caller-owned mint inputs, allocates one Server operation ID, derives `fresh_session_id`, and sends them on the internal envelope.
2. The Runner serializes the container transition, rejects an already-live container, and starts the agent with that ID and attempt token.
3. The Server requires an exact `fresh_session_id` echo, exact operation-ID echo, and exact authenticated container-attempt match before promoting the account binding and returning the public `session_id`.
4. A mismatch is version skew or a Server/Runner defect. The Server does not promote it. Cleanup is authorized by the authenticated container-attempt fence, not by the mismatched reported ID: it may stop only the exact attempt that performed this operation, using a bounded context independent of the cancelled start request. The reported ID is diagnostic only and is never used to select a foreign or later generation. The Server returns an internal error naming both IDs; if fenced cleanup fails, the error log names the container, operation, attempt, expected ID, and reported ID for operator cleanup.

A missing result variant, missing echo, or missing attempt correlation is a wire-contract error and fails without Stop because ownership cannot be proven. A fresh start with no Server-minted `fresh_session_id` fails at the Runner with `FailedPrecondition`; it never falls back to a Runner-local counter.

### Resume (planned target behavior; pending implementation PR)

Resume authorization happens before the Runner call. The authorized logical public `session_id` is reused as the live ID; no fresh ID or new binding is created. The Runner materializes the reconstructed session body before agent exec, validates the resume ID as a bare path element, and returns an exact resume-ID and authenticated attempt echo. The Server verifies both echoes and the same operation/attempt fence before accepting the resume. A live collision for the logical ID returns `AlreadyRunning`. A fresh ID or caller operation ID supplied alongside a resume is rejected, not selected or silently replaced.

## ID construction and retry identity

Shipped baseline (`a92fa2d0`): the Runner-local `monotonicIDs` generator emits
`sess-1`, `sess-2`, and so on within one process. The counter resets after a
Runner restart, and the baseline does not provide deterministic retry identity
across restarts.

Planned target behavior (pending the implementation PR) follows DL-371 exactly.
The Server derives `fresh_session_id` from a **Server-generated relay request ID**
at the mint boundary. A caller-provided `request_id` or `orNewRequestID` input is
never accepted as authority for a fresh ID: the Server MUST reject a non-empty
caller value (or ignore it and replace it with a newly generated value before
derivation, as the public API contract for that relay specifies), and production
must never let a client choose the normalized ID. The Runner has no minting or
fallback path. The input is domain-separated, length-prefixed, and SHA-256
hashed:

```text
SHA256("compass.session-id.v1" ||
       u32be(len(server_relay_request_id)) || server_relay_request_id)
```

The digest is encoded as 64 lowercase hexadecimal characters. The domain label
and length prefix are frozen by DL-371. No container name, account ID, or mutable
retry metadata is included. A retry that reuses the same normalized Server
relay request ID therefore derives the same logical ID. A distinct request ID
derives a distinct ID.

The Server's mint boundary also owns the cross-container uniqueness invariant:
every fresh generation, regardless of destination container, receives a
distinct Server relay request ID and therefore a distinct derived session ID.
A request routed to another container MUST use a fresh relay request ID; the
same ID MUST NOT be reused across containers or made into a cross-container
selector. The Runner receives the derived value and never derives or
substitutes one.

The request ID is stable only for retries of one Server operation. Concurrent
generations use distinct relay request IDs and therefore distinct IDs; the
per-container transition lock remains the independent generation fence.

Known-vector proof is required in the implementation PR, not just a property
claim: add `TestFreshSessionIDKnownVector` in `go/internal/runnerhub` and run
`go test ./internal/runnerhub -run '^TestFreshSessionIDKnownVector$' -count=1`.
The test must assert the exact lowercase digest for a fixed request ID and also
assert same-input equality plus distinct-input inequality.

The shipped baseline remains unchanged: `a92fa2d0` still uses Runner-local
`monotonicIDs` until this proposal lands.

## Mixed-version compatibility and rollout ordering

The planned target behavior (pending the implementation PR) has no Runner
version handshake. The Runner-first capability gate deploys the Server
mint-and-echo path only after the target Runner accepts and returns
`fresh_session_id`; otherwise fresh starts fail before invoking the Runner.
Mixed-version smoke covers target Server + old Runner (mismatch, no binding,
fenced cleanup), old Server + target Runner (fresh `FailedPrecondition`), and
target Server + target Runner (fresh and resume success). Resumes remain
compatible because the Runner gives precedence to the authorized
`resume_session_id`; no silent fallback minting is allowed.

## Acceptance tests (planned target behavior; pending implementation PR)

The implementation PR is accepted only when these deterministic commands pass. Each test names an observable contract; the shipped baseline at `a92fa2d0` remains Runner-local `monotonicIDs` until that PR lands.

- `go test ./internal/runnerhub -run '^TestFreshSessionIDKnownVector$' -count=1` — assert the exact DL-371 lowercase digest for a fixed Server operation ID, same-operation retry equality, and distinct-operation inequality.
- `go test ./internal/runnerhub -run '^TestFreshSessionIDRejectsCallerInput$' -count=1` — a caller-supplied request/fresh/account/attempt value is rejected before minting; the Server-owned operation ID is the only derivation input.
- `go test ./internal/runnerhub -run '^TestFreshStartEnvelopeCarriesServerOperationAndFreshID$' -count=1` — the internal envelope carries the Server operation ID, derived `fresh_session_id`, and attempt fence, while the public request has no mint fields.
- `go test ./internal/runnerhub -run '^TestStartRelayRequiresExactIDAndAttemptEcho$' -count=1` — exact ID, operation, and authenticated attempt echoes are required before one binding is promoted; missing or altered values return `Internal` and create no binding.
- `go test ./internal/runnerhub -run '^TestMismatchCleanupUsesAttemptFenceNotReportedID$' -count=1` — matching attempt correlation permits bounded cleanup under a detached context; foreign, unbound, later-generation, and mismatched-attempt IDs are never passed to Stop.
- `go test ./internal/runnerhub -run '^TestRetryReusesOperationButRerouteMintsNewOperation$' -count=1` — retrying the same Server operation reuses its ID and logical session ID; rerouting to another container allocates a new operation and cannot reuse the old ID as a cross-container selector.
- `go test ./internal/runnerhub -run '^TestRelayCommsCallAttributesToBoundAccount$' -count=1` && `go test ./internal/runnerhub -run '^TestRelayForgeCallAttributesToBoundAccount$' -count=1` && `go test ./internal/runnerhub -run '^TestRelayBoardCallAttributesToBoundAccount$' -count=1` && `go test ./internal/runnerhub -run '^TestRelayLifecycleCallAttributesToBoundAccount$' -count=1` && `go test ./internal/runnerhub -run '^TestFetchSecretsAttributesToBoundAccount$' -count=1` — every account-scoped relay resolves only through the Server-owned binding.
- `go test ./internal/runnerhub -run '^TestFailClosedStoppedNeverSeenAndPostReconnect$' -count=1` — stopped, unknown, and stale post-reconnect IDs resolve to no account.
- `go test ./internal/runnerhub -run '^TestEnrollFiresReapSinkWithClearedSessionIDs$' -count=1` && `go test ./internal/runnerhub -run '^TestConcurrentResolveDuringAFaultingReapCannotResurrect$' -count=1` — enrollment and concurrent reap cannot resurrect a stale binding.
- `go test ./internal/runner -run '^TestFreshStartRejectsMissingServerID$' -count=1` — the Runner returns `FailedPrecondition` and never invokes a local fallback generator.
- `go test ./internal/runner -run '^TestResumeExactSessionAndAttemptEcho$' -count=1` — resume returns the authorized public `session_id` exactly, validates the attempt echo, materializes the body before exec, and creates no fresh binding.
- `go test ./internal/runner -run '^TestResumeRejectsCallerFreshIDAndMalformedSessionID$' -count=1` — caller mint inputs and path-escaping resume IDs fail closed; a live collision returns `AlreadyRunning`.
- `go test ./internal/runnerhub -run '^TestMixedVersionRunnerFirstCapabilitySmoke$' -count=1` — old/new Server and Runner permutations prove fresh precondition or echo mismatch, fenced cleanup, no binding on skew, and usable exact-ID resumes.

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

## Alternatives rejected

- **Runner-local random or enrollment-prefixed IDs:** collision resistance does not change the authority problem; the Runner would still mint a Server-owned key.
- **Repair `ErrConflict`:** deleting or repointing durable ownership on an error path could steal an account and would hide the collision.
- **Public `fresh_session_id`:** a client-chosen storage key is an authorization bypass vector. The internal envelope is the only carrier.
- **Promote whatever the Runner returns:** this silently accepts old-version skew. Echo verification plus bounded, ownership-fenced cleanup makes skew loud and fail closed.
- **Per-call random IDs or request ID verbatim:** the former breaks retry idempotency; the latter conflates transient correlation identity with a durable public key.

## Implementation references

- `go/internal/runnerhub/commands.go`: fresh-start relay, request identity, and promotion.
- `go/internal/runnerhub/relay_comms.go`: authoritative live binding and conflict behavior.
- `go/internal/runner/dispatch.go`: envelope-to-host threading and error map.
- `go/internal/runner/host.go`: per-container lock, ID selection, resume path, and pre-exec materialization.
- `go/internal/store/agent_sessions.go`: durable ownership and subscriber authorization.

The implementation changes Go and generated internal protocol surfaces; this record changes documentation only.
