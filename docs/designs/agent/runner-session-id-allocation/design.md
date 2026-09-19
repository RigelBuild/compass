# Runner session-ID allocation: server-minted fresh IDs (RIG-3696)

Status: Active proposal

Issue: RIG-3696 (P1, parent RIG-2861).
This record proposes a server-minted, restart-safe session-ID change. It is not an authoritative description of the currently shipped implementation. The shipped baseline remains Runner-local, emits process-local values, and resets its counter after a Runner restart. The design is frozen on merge; later changes supersede it by citation.

## Problem and intent

A fresh `StartAgentSession` must not reuse an identifier after a Runner restart. The old Runner-local `monotonicIDs` counter restarted from zero in each process, while durable session and binding rows survived. A reused ID could make a new lifetime resolve to an old account. The security invariant is therefore:

> A `session_id` selects a Server-owned account binding. It never carries an account, and a fresh lifetime never reuses a prior lifetime's ID.
Terminology is fixed throughout this record: the **relay request ID** is the Server-generated input to DL-371 derivation; the **operation ID** identifies one Server-to-Runner operation and its retry record; the **caller request ID** is any client-supplied correlation value and is never a mint input. The public `session_id` is the durable logical key returned after promotion. The internal `fresh_session_id` is the Server-to-Runner value for a new lifetime and is never caller-selectable.

The planned target behavior, pending the implementation PR, makes the Server the authority for a fresh ID. The Runner receives that `fresh_session_id` on the internal `SessionsResponse` envelope, never mints a fallback, and echoes the selected ID in its start result. A resume reuses the already-authorized logical `resume_session_id`; it does not create a second logical session. The shipped baseline does not yet provide this server-minted envelope path.

## Decisions

### Security contract gap closure: planned wire and authorization changes

The current wire is not sufficient for the target behavior: `SessionsResponse` has only `request_id`, its start command uses the public start request, and the request/response pair lacks the internal result fields required below. The implementation PR MUST make these additive, internal-only contract changes without changing the shipped baseline or the DL-371 derivation:

| Message | Required field (tag) | Contract |
| --- | --- | --- |
| `SessionsResponse` | `string operation_id = 14` | Server-generated relay operation identity. Required on `start`, `resume`, and `stop`; never copied from a public request and never accepted from a client. |
| `SessionsResponse` | `string fresh_session_id = 15` | Server-derived ID for a fresh start only. Required and non-empty for fresh `start`; absent for `resume` and other commands. The Runner MUST reject a fresh start that omits it. |
| `SessionsResponse` | `string container_attempt_id = 16` | Authenticated, per-container-generation fence issued by the Server/Runner enrollment. Required on `start`, `resume`, and `stop`; never caller-selectable. |
| `SessionsRequest` | `string operation_id = 9` | Exact echo of the command envelope's Server operation ID. Missing or altered values are a wire-contract failure. |
| `SessionsRequest` | `string container_attempt_id = 10` | Exact authenticated attempt echo. The Server compares it with the enrollment and command envelope before promotion or cleanup. |
| `StartAgentSessionResponse` | `string fresh_session_id = 2` | Exact echo of the internal fresh ID; populated only for fresh start and not surfaced by the public RPC response. |
| `StartAgentSessionResponse` | `string resume_session_id = 3` | Exact echo of the authorized resume ID; populated only for resume. |
| `StopAgentSessionRequest` | `string container_attempt_id = 2` | Internal Stop envelope fence. The public Stop RPC MUST reject a non-empty value; the Server populates it only after resolving the durable binding. |

Tags 9/10 on the request envelope and 14-16 on the response envelope are reserved for this contract even if generated names differ. If reusing a public message would expose an internal field, introduce equivalent internal-only messages at fresh tags instead; the field names, cardinality, and exact-echo rules above remain mandatory. No caller request ID, account ID, actor ID, container name, or attempt ID is a mint input.

Before dispatch, the Server authorizes the logical resume ID against the
durable binding tuple `(session_id, account_id, actor_id, container_name)` and
the authenticated Runner enrollment. The actor is the account's authorized
subscriber, not a value supplied by the Runner. The Server records the
operation's account, actor, container, and attempt and requires all four to
match on result, resume, relay calls, and Stop. A valid session ID from a
different account or container returns `NotFound`/`PermissionDenied` without
revealing which tuple component failed. A stale attempt after reconnect is
also rejected. This is the authorization root; in-memory caches are only a
disposable acceleration layer.

### Authoritative binding and data flow

Planned target behavior (pending the implementation PR): the Server is the sole fresh-start minting boundary. It creates the Server-owned relay operation ID, derives `fresh_session_id`, and sends both on the internal envelope. The Runner returns the selected ID and authenticated attempt metadata in its start result. The Server promotes the provisioned account binding only after exact echo and attempt validation. Account-scoped operations resolve the account only from that Server-owned binding.

The internal `fresh_session_id` is not a public request input and is never caller-selectable. The public `session_id` is the logical, durable key returned after promotion and used by later account-scoped calls. The internal envelope also carries the Server-owned operation ID and exact container-attempt token needed for correlation. A caller-supplied request ID, fresh ID, account ID, or attempt token is rejected before minting.

### Fresh start (planned target behavior; pending implementation PR)

1. The Server validates that the public request contains no caller-owned mint inputs, allocates one Server operation ID, derives `fresh_session_id`, and sends them on the internal envelope.
2. The Runner serializes the container transition, rejects an already-live container, and starts the agent with that ID and attempt token.
3. The Server requires an exact `fresh_session_id` echo, exact operation-ID echo, and exact authenticated container-attempt match before promoting the account binding and returning the public `session_id`.
4. A mismatch is version skew or a Server/Runner defect. The Server does not promote it; cleanup follows the exact internal lookup and attempt fence in the mismatch-correlation section, with the reported ID diagnostic only.

A missing result variant, missing echo, or missing attempt correlation is a wire-contract error and fails without Stop because ownership cannot be proven. A fresh start with no Server-minted `fresh_session_id` fails at the Runner with `FailedPrecondition`; it never falls back to a Runner-local counter.

### Resume (planned target behavior; pending implementation PR)

Resume authorization happens before the Runner call. The authorized logical public `session_id` is reused as the live ID; no fresh ID or new binding is created. The Runner materializes the reconstructed session body before agent exec, validates the resume ID as a bare path element, and returns an exact resume-ID and authenticated attempt echo. The Server verifies both echoes and the same operation/attempt fence before accepting the resume. A live collision for the logical ID returns `AlreadyRunning`. A fresh ID or caller operation ID supplied alongside a resume is rejected, not selected or silently replaced.

## ID construction and retry identity

The shipped baseline uses Runner-local process counters. The counter resets after a Runner restart, and the baseline does not provide deterministic retry identity across restarts.

The input is domain-separated, length-prefixed, and SHA-256 hashed:

```text
SHA256("compass.session-id.v1" ||
       u32be(len(server_relay_request_id)) || server_relay_request_id)
```

The digest is encoded as 64 lowercase hexadecimal characters. The domain label
and length prefix are frozen by DL-371. No container name, account ID, or
mutable retry metadata is included. A retry that reuses the same normalized
Server relay request ID therefore derives the same logical ID. A distinct
request ID derives a distinct ID.

The Server's mint boundary also owns the cross-container uniqueness invariant:
every fresh generation, regardless of destination container, receives a
distinct Server relay request ID and therefore a distinct derived session ID.
A request routed to another container MUST use a fresh relay request ID; the
same ID MUST NOT be reused across containers or made into a cross-container
selector. The Runner receives the derived value and never derives or
substitutes one.

### Retry, persistence, and crash recovery

The Server persists one operation row before dispatch. It contains the relay request ID, derived `fresh_session_id`, opaque retry handle (stored hashed), authenticated tuple `(account_id, actor_id, runner_id, container_name, container_attempt_id)`, current enrollment revision, and state. Unique constraints on the operation identity, relay ID, and handle digest prevent duplicate ownership; the promoted binding has a unique logical `session_id`. The handle is the only retry selector and is never treated as an operation ID.

The states are **in flight**, **promoted**, and **failed**. Create, handle lookup/join, and promotion are single database transactions. A retry is accepted only when its authenticated tuple and current enrollment equal the persisted tuple. Unknown, foreign, stale, and expired handles all return the same external `NotFound`; the response does not reveal which check failed.

- While **in flight**, a valid handle joins the existing operation atomically. It never allocates another operation or ID, reroutes, or cancels the first dispatch.
- Once **promoted**, a valid handle returns the recorded public result without calling the Runner or writing another binding.
- A **failed** operation is retryable only when its recorded failure is explicitly retryable and no binding was promoted. Otherwise the same external `Conflict` is returned; reroute uses a new operation and relay ID.

The dispatch worker records the result and promotes only after exact ID, operation, and attempt echoes. If the Server crashes after dispatch but before promotion, recovery scans in-flight rows, verifies the recorded enrollment and attempt, and either joins/reconciles the same operation or marks it failed without creating a second binding. Recovery never promotes from a reported ID alone. Any uniqueness conflict is a Server-bug witness: return external `Conflict` and leave existing ownership untouched.

Rerouting after promotion is a new operation and must not reuse the promoted operation or logical ID. It returns `AlreadyRunning` when the account already has a live binding. A request routed to another container always receives a new relay request ID; relay IDs and derived fresh IDs are never cross-container selectors.

The request ID is stable only for retries of one operation. Concurrent generations use distinct relay IDs. The per-container transition lock remains the independent generation fence.

The shipped baseline remains unchanged until this proposal lands.

## Mixed-version compatibility and rollout ordering

The planned target behavior (pending the implementation PR) uses an explicit
**deployment gate**, not a vague Runner-first capability guess and not a
runtime version handshake. The authoritative revision source is the immutable
release manifest, not a Runner-reported capability string or mutable runtime
handshake. Each signed Server artifact and Runner image MUST publish the same
`session_id_wire_contract_revision` value (the frozen target is
`runner-session-id-v1`). The gate reads the manifest digest and revision from
the artifact it is about to start, verifies signature/digest, and compares both
values to the expected target. Missing, unsigned, unverifiable, or mismatched
metadata is an admission failure. A Runner may report its revision for
diagnostics, but that report cannot raise or override the manifest value.
Readiness and fresh-start admission fail closed before serving traffic and name
expected revision, manifest revision, and artifact digests without accepting a
partial pair.

`TestDeploymentGateUsesImmutableManifestRevision` feeds the gate a matching
signed manifest, missing revision, forged/mismatched revision, and valid old
revision. Only the exact target pair becomes ready; every other case rejects
fresh starts before a Runner call and creates no binding.

The rollout admits the pair only when both revisions are the target revision that defines `SessionsResponse.fresh_session_id` and the exact operation and attempt echoes. A mismatched pair is observably rejected at readiness/startup with a protocol-revision error naming expected and reported revisions; it is not admitted to serve fresh starts. There is no mixed-version serving window.

## Acceptance tests (planned target behavior; pending implementation PR)

The implementation PR is accepted only when deterministic tests prove these observable contracts. The shipped baseline at `a92fa2d0` remains Runner-local until that PR lands.

- A known-vector test asserts the exact DL-371 lowercase digest for a fixed Server relay request ID, same-operation retry equality, and distinct-operation inequality.
- A caller-input test rejects non-empty caller request, fresh, account, or attempt values before minting; only the Server-owned operation identity supplies derivation input.
- An envelope test proves the internal message carries the Server operation ID, derived `fresh_session_id`, and attempt fence, while public requests cannot set internal fields.
- An echo-validation test requires exact ID, operation, and authenticated attempt echoes before one binding is promoted; missing or altered values return an internal error and create no binding.
- A cleanup test proves matching attempt correlation permits bounded cleanup under a detached context; foreign, unbound, later-generation, and mismatched-attempt IDs are never passed to Stop.
- A retry test proves valid same-operation joins and post-promotion replay; unknown, foreign-account, stale-attempt, and old-enrollment handles all produce the same external `NotFound`, and non-retryable failures produce `Conflict`.
- A persistence test kills the Server after dispatch and before promotion, restarts recovery, and proves at most one binding and one public result; rows retain relay ID, derived ID, retry-handle digest, full tuple, enrollment revision, and state.
- A deterministic concurrency test releases two same-container starts at the transition barrier and proves exactly one is accepted, the other returns `AlreadyRunning`, and no second binding or fresh ID is created.
- Account-attribution tests prove every account-scoped relay and secret operation resolves only through the Server-owned binding.
- Fail-closed lifecycle tests prove stopped, unknown, and stale post-reconnect IDs resolve to no account, and enrollment/reap cannot resurrect stale bindings.
- Runner tests prove a fresh start without a Server ID returns `FailedPrecondition` without local fallback, while resume returns the authorized public ID exactly, validates its attempt echo, materializes the body before exec, and creates no fresh binding.
- Resume validation tests reject caller mint inputs and path-escaping IDs; a live collision returns `AlreadyRunning`.
- Deployment-gate tests reject mismatched protocol revisions before fresh starts and bindings, while the target pair supports fresh start and resume.

### Lifecycle and persistence boundary

`StartAgentSession` is the lifecycle operation. Its durable session and transcript contract is defined by [the session-persistence design](../compass-agent-session-persistence/design.md); this record adds only operation identity, enrollment fencing, and promotion rules. The logical session ID remains stable across authorized resume, while each live lifetime is tied to the serving Runner and exact container attempt.

Runner enrollment marks the prior attempt stale and clears its live cache. A stopped, unknown, or post-reconnect ID fails closed rather than inheriting an old account. The durable row is the authorization root; Hub/Runner maps are only a dispatch cache. Cache loss is recoverable from the durable row, but row absence or attempt mismatch is fail-closed.

The Runner holds the per-container transition lock across pre-exec work and checks for a live session before selecting an ID. Start, resume, stop, remove, reload, and reconnect use the same transition discipline. Old-lifetime frames cannot bind to a new lifetime; lifetime fencing is independent of the logical session ID.

| Event | Durable store (authoritative) | Hub/Runner cache (disposable) |
| --- | --- | --- |
| Enrollment | Mark the prior attempt stale. | Clear old live bindings; accept only the new attempt. |
| Fresh start | Promote after exact echoes and attempt match. | Populate only after promotion. |
| Reconnect | Keep logical session and transcript; do not rewrite ownership. | Drop old-attempt bindings, rebuild from authenticated state. |
| Crash/restart | Preserve operation/session/transcript rows; create no binding without promotion. | Discard pre-crash entries. |
| Stop/Remove | Release the matching binding under the lifecycle operation. | Remove only the matching attempt entry. |

A returned-ID mismatch is handled only after correlating the response to the
same authenticated Server operation, Runner enrollment, account, actor,
container name, and container-attempt token that performed the start. Before
any side effect, cleanup performs an exact internal lookup keyed by
`(operation_id, account_id, actor_id, runner_id, container_name,
container_attempt_id)` against the operation record, durable binding, and
current enrollment. Cleanup has one selector: the internally recorded target
returned by that lookup.

If the exact lookup is absent or differs, the Server records the mismatch and
takes no cleanup action. When it matches, the Server builds the attempt-bearing
Stop envelope from the lookup result and may stop only that exact
operation/container/attempt. It MUST NOT stop a foreign ID, a later
generation, or an ID belonging to another Runner/container pair. The reported
session ID is diagnostic only: it is never placed in Stop, never used to select
a cleanup target, and never a fallback when the internal lookup fails. Cleanup
uses a bounded context independent of the cancelled start request. The Server
never promotes the mismatched result; if cleanup fails, the log names the
correlated operation, Runner, account, actor, container, attempt, expected ID,
and reported ID for operator cleanup.

This same rule applies to mixed-version responses: an old Runner's returned
local ID is never promoted and remains diagnostic only. It is never passed to
Stop, even when the authenticated attempt fence proves which attempt ran.
