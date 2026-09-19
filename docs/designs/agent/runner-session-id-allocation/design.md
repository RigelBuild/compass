# Runner session-ID allocation: server-minted fresh IDs (RIG-3696)

Status: Active

Issue: [RIG-3696](https://linear.app/rigelbuild/issue/RIG-3696) (P1, parent RIG-2861).
This record is the public contract for the shipped restart-safe session-ID change.
It supersedes the Runner-local counter and the unmerged alternatives described in
PRs #1280 and #1283. The design is frozen on merge; later changes supersede it by
citation.

## Problem and intent

A fresh `StartAgentSession` must not reuse an identifier after a Runner restart.
The old Runner-local `monotonicIDs` counter restarted from zero in each process,
while durable session and binding rows survived. A reused ID could make a new
lifetime resolve to an old account. The security invariant is therefore:

> A `session_id` selects a Server-owned account binding. It never carries an
> account, and a fresh lifetime never reuses a prior lifetime's ID.

The Server is the authority for a fresh ID. The Runner receives that ID on the
internal `SessionsResponse` envelope and never mints a fallback. A resume reuses
the already-authorized logical `resume_session_id`; it does not create a second
logical session.

## Authoritative binding and data flow

`Hub.Start` is the sole fresh-start minting boundary. It sends
`fresh_session_id` on the internal envelope, `dispatcher.execute` passes it to
`agentHost.Start`, and the Runner returns the selected ID in its start result.
The Server promotes the container's provisioned account binding only after that
result. `promoteSession` records the live `session_id` to account binding and
keeps its existing fail-closed behavior for unknown accounts. RelayCommsCall,
RelayForgeCall, RelayBoardCall, RelayLifecycleCall, and FetchSecrets resolve the
account from that binding; no relay trusts an account asserted by the Runner or
client (`go/internal/runnerhub/relay_comms.go`, `relay_forge.go`,
`relay_board.go`, `relay_lifecycle.go`, `handler.go`).

The internal field is a sibling of `request_id` and `resume_body`, outside the
command oneof. It is not added to the public `StartAgentSessionRequest`.
Fresh starts carry the Server-minted field. Resumes carry the existing
`resume_body` and no fresh ID. The Runner selects `resume_session_id` first;
otherwise it requires a non-empty `fresh_session_id` and uses it verbatim.

### Fresh start

1. `Hub.Start` derives a fresh ID and sends it with `Start`.
2. The Runner serializes the container transition, rejects an already-live
   container, and starts the agent with that ID.
3. The Server verifies the returned ID is the one it sent, then promotes the
   account binding and returns the response.
4. A returned-ID mismatch is version skew or a Server/Runner defect. The Server
   does not promote it. It attempts a bounded Stop for the Runner-reported ID
   and returns an internal error naming both IDs. If Stop fails, the error log
   names the container and both IDs for operator cleanup.

A missing result variant is also a wire-contract error and fails without Stop:
there is no returned ID to stop. A fresh start with no Server ID fails at the
Runner with `FailedPrecondition`; it never falls back to a local counter.

### Resume

Resume authorization happens before the Runner call. The authorized logical ID
is reused as the live ID. The Runner materializes the reconstructed session
body before agent exec, validates the resume ID as a bare path element, and
rejects malformed path input. A live collision for the logical ID returns
`AlreadyRunning`. A fresh ID supplied alongside a resume is ignored in favor of
the resume ID and logged as skew; production never sends both.

## ID construction and retry identity

The fresh ID is 64 lowercase hexadecimal characters: a SHA-256 digest of
length-prefixed fields in this order:

`"compass.session.v1"`, the envelope `request_id`, and the target
`container_name`.

The domain-separated construction is shared with the existing provision
correlation derivation. `request_id` is normalized once by `orNewRequestID` and
is used both in the envelope and in the derivation. Thus a retry or concurrent
router join that reuses a request ID receives the same session ID, while the
same request ID aimed at a different container cannot share one. The ID is not
the request ID verbatim and has no `sess-` prefix.

The Server's derivation is authoritative even though the value is deterministic:
production callers cannot supply the request ID, and the internal envelope is
not client-addressable. If a future public request gains a client-chosen request
ID, this derivation must be revisited before that field is used for minting.

## Lifetime, generation, and locking fences

The logical session ID is stable across authorized resumes. A live lifetime is
still tied to the serving Runner and container. Runner enrollment clears the
serving Runner's live bindings; a stopped, unknown, or post-reconnect ID fails
closed rather than inheriting an old account. The Server's durable row remains
the authorization root, while the Hub's in-memory maps are the live dispatch
cache.

`agentHost.Start` takes the per-container transition lock for the complete
start, including the slow pre-exec work. Under the Runner mutex it checks for a
live session on that container before selecting the ID. This closes the
check/start race that could otherwise admit two generations. Resume selection
also checks that the logical ID is not already live. Stop, Remove, reload, and
reconnect use the same container/session transition discipline. A future
multi-lifetime implementation must preserve the invariant that an old
lifetime's frames cannot bind to a new lifetime; it must use an explicit
lifetime/generation fence rather than infer one from process uptime.

The durable transcript path (when enabled by the session-persistence design)
keeps sequence state scoped to the logical session and re-bases each Runner
lifetime. The session ID remains the stable key; a lifetime/generation marker is
not a client-selectable substitute for that key.

## Fail-closed boundaries and resource bounds

The implementation fails closed at every trust boundary:

- empty fresh IDs return `FailedPrecondition`; no Runner-local minting exists;
- a missing start result returns an internal wire-contract error without an
  invented ID;
- a Runner echo mismatch is never promoted and triggers a bounded Stop;
- unknown or foreign session IDs resolve to no account;
- malformed resume IDs cannot escape the Runner's resume directory;
- resume bodies are materialized before exec, and a body without a resume ID is
  dropped with a warning rather than attached to a fresh session;
- `ErrConflict` in `promoteSession` remains a Server-bug witness, not a repair
  path; it is logged and the cache fallback remains unchanged;
- the mismatch Stop uses a bounded timeout and a context independent of the
  cancelled start request, so a stuck Runner cannot hold the RPC forever.

The session-ID change adds no unbounded retry, archive, or body buffering. It
does not broaden resume-body size or storage limits. Those limits remain owned
by the session-persistence implementation: reconstructed bodies use the
authoritative hot tail, archives are fallback history, and archive/resource
failure must fail closed rather than fabricate a partial loadable transcript.

## Mixed-version compatibility

There is no Runner version handshake. Deploy Server and Runner together.

- A newer Runner with an older Server receives no fresh ID and rejects every
  fresh start with `FailedPrecondition`.
- An older Runner ignores the internal fresh-ID field and returns its old local
  ID. The newer Server detects the mismatch, attempts bounded cleanup, and
  returns `Internal` without recording a binding.
- Resumes remain compatible in either skew because the Runner gives precedence
  to the authorized `resume_session_id`.

This is deliberate partial availability, not a compatibility shim. A deployment
must not roll only one side and wait for fresh starts to recover.

## Alternatives rejected

- **Runner-local random or enrollment-prefixed IDs:** collision resistance does
  not change the authority problem; the Runner would still mint a Server-owned
  key.
- **Repair `ErrConflict`:** deleting or repointing durable ownership on an error
  path could steal an account and would hide the collision.
- **Public `fresh_session_id`:** a client-chosen storage key is an authorization
  bypass vector. The internal envelope is the only carrier.
- **Promote whatever the Runner returns:** this silently accepts old-version
  skew. Echo verification plus bounded cleanup makes skew loud and fail closed.
- **Per-call random IDs or request ID verbatim:** the former breaks retry
  idempotency; the latter conflates transient correlation identity with a
  durable public key. Domain-separated hashing preserves both boundaries.

## Acceptance criteria

The following are the public, observable contract:

1. A fresh start returns a non-empty 64-hex ID derived by the Server, and the
   Runner reports that exact ID; no Runner-local ID generator remains.
2. Same-request retries and concurrent joins return one stable ID; a different
   target container cannot derive that ID.
3. A resume returns its authorized logical ID, materializes its body before
   exec, and does not create or record a fresh ID.
4. Concurrent starts on one container cannot create two live generations; a
   resume cannot displace an existing live lifetime.
5. Missing, malformed, foreign, mismatched, or otherwise untrusted IDs fail
   closed. A mismatch attempts bounded cleanup and records no binding.
6. All account-scoped relay paths resolve through the Server-owned binding, and
   reconnect/Stop/Remove release that binding so stale IDs cannot authorize.
7. Server and Runner mixed-version behavior is explicit: fresh starts fail
   precondition or internal mismatch; resumes remain usable; no silent fallback
   minting occurs.
8. Tests cover the fresh-ID envelope, retry identity, missing-ID precondition,
   resume precedence and path guard, per-container start lock, mismatch cleanup,
   and stale-binding rejection. Public documentation names capabilities and
   trust boundaries only; it does not expose deployment-specific secrets or
   private service details.

## Implementation references

- `go/internal/runnerhub/commands.go`: fresh-start relay, request identity,
  mismatch handling, and bounded Stop.
- `go/internal/runnerhub/resume_start.go`: resume envelope and promotion.
- `go/internal/runner/dispatch.go`: envelope-to-host threading and error map.
- `go/internal/runner/host.go`: per-container lock, ID selection, resume path,
  and pre-exec materialization.
- `go/internal/runnerhub/relay_comms.go`: authoritative live binding and
  conflict behavior.
- `go/internal/store/agent_sessions.go`: durable ownership and subscriber
  authorization.

The implementation changes Go and generated internal protocol surfaces; this
record changes documentation only.
