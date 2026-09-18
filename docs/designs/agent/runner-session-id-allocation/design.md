# Runner session-id allocation: server-minted fresh session IDs (RIG-3696)

Status: Draft

Issue: [RIG-3696](https://linear.app/rigelbuild/issue/RIG-3696) (P1, parent
RIG-2861). Supersedes the Runner-local random-id fix in PR #1280 and the
unreviewed server-minted draft in PR #1283; this record is the contract both
are measured against.

## Problem / Intent

A fresh `StartAgentSession` gets its live `session_id` from the Runner's
process-local counter (`monotonicIDs` in `go/internal/runner/host.go`:
`"sess-" + strconv.FormatUint(n, 10)`, `n` starting at 0 in every Runner
process). A Runner restart re-mints `sess-1` while the Server still holds the
durable `agent_sessions` and `session_bindings` rows the *previous* `sess-1`
created for a *different* agent account. `Hub.promoteSession`
(`go/internal/runnerhub/relay_comms.go`) then gets `store.ErrConflict` from
`RecordSessionBinding`, logs it, and falls back to the in-RAM map — so the
durable binding row keeps naming the stale account while the four relay arms
(`accountForSession`, consumed by `relay_comms.go`, `relay_forge.go`,
`relay_board.go`, `relay_lifecycle.go`) and `FetchSecrets` may resolve the new
session to the old account on any instance that reads durable truth. That is
the cross-account secret disclosure RIG-3696 names. The fix Matt ruled: the
**Server mints every fresh session id** as a collision-free random value and
carries it to the Runner on the internal `Sessions` envelope; the Runner stops
minting. Resume keeps reusing its authorized `resume_session_id` as the live
id. Nothing is deleted downstream and `ErrConflict` is not handled as a
recovery path.

## Approach

**Narrow server-minted envelope field.** Add one INTERNAL-ONLY string field,
`fresh_session_id`, to `compass.v1.SessionsResponse` in
`proto/compass/v1/runner.proto` as a top-level sibling of `request_id` and
`resume_body`, outside the `command` oneof — the exact shape `resume_body = 13`
already uses for Server-attached, never-client-suppliable data. The Server sets
it only on a fresh start; the Runner selects
`resume_session_id` first, else `fresh_session_id`, and fails closed when
neither is present. The public `StartAgentSessionRequest` is relayed verbatim
and gains no field: a client must never choose the id its session is keyed by.

Why this shape and not a Runner-local random id (PR #1280's approach): a random
id minted on the Runner is also collision-free, but it leaves the Runner as the
authority for a key the Server persists, authorizes, and binds accounts to.
Every existing comment on that key already says "server-minted"
(`RecordAgentSession` doc: "session_id is the server-minted response";
`agent_sessions.go` header: "Rooted non-spoofably (session_id is server-minted
post-Runner-call)"). Minting on the Server makes the code match the stated
trust model and lets the Server assert, before the Runner runs anything, that
the id it will record is one it chose.

### Data flow after the change

```mermaid
sequenceDiagram
    participant C as Client
    participant S as service.StartAgentSession
    participant H as Hub.Start / Hub.StartResume
    participant R as runner.dispatcher
    participant A as agentHost.Start
    C->>S: StartAgentSessionRequest{container_name[, resume_session_id]}
    alt fresh
        S->>H: Start(req)
        H->>H: rid = orNewRequestID(""); id = mintSessionID(rid, container_name)
        H->>R: SessionsResponse{request_id=rid, start=req, fresh_session_id=id}
    else resume
        S->>H: StartResume(req, body)
        H->>R: SessionsResponse{start=req, resume_body=body}
    end
    R->>A: Start(req, resumeBody, freshSessionID)
    A->>A: sessionID = resume_session_id || freshSessionID || error
    A-->>R: sessionID
    R-->>H: SessionsRequest{start{session_id}}
    H->>H: promoteSession(container, session_id)
    H-->>S: StartAgentSessionResponse{session_id}
    S->>S: RecordAgentSession(session_id, account) [fresh only]
```

### Decisions

1. **Field placement and tag.** `string fresh_session_id = 14;` on
   `SessionsResponse`. Tag 13 is `resume_body`; 12 is skipped (DL-065's
   abandoned `ResumeContext resume = 12`, never wired — the existing comment on
   tag 13 records that 12 is not reused). 14 is the next free tag. Field
   numbers under 16 encode in one byte; a rarely-set string costs nothing when
   empty (proto3 omits zero values).
2. **Id shape: derived from the request id and container name (Matt's ruling).** The fresh
   session id is bound to the Sessions envelope's `request_id` so a retry that
   reuses the request id also reuses the session id, preserving the
   idempotency the envelope documents ("`request_id` is the OQ6 idempotency
   key: a retry reuses it and the Runner returns the original result" —
   `dispatcher.handle` returns the recorded first result; `commandRouter.dispatch`
   joins a concurrent same-id call to the in-flight one) — and to the target
   `container_name`, so one request id replayed against another container
   cannot derive the same session id (Matt's ruling). Derivation, in
   `mintSessionID(requestID, containerName string) string`:
   `hex(sha256(len-prefixed "compass.session.v1" || len-prefixed requestID || len-prefixed containerName))`
   — the exact domain-separated, length-prefixed construction
   `provisionDedupID` already uses, so no field can be shifted into another
   and no request id can land in another domain's id space. Output is 64
   lowercase hex chars,
   no `sess-` prefix (no non-test code reads the prefix; grep `"sess-"` in
   `go/` finds only `monotonicIDs` and fixtures). The 64-hex id is a bare
   path element, so the Runner's `filepath.Base` resume-id guard is
   unaffected on a later resume. Entropy is inherited from the request id:
   `Hub.Start` mints the request id itself with `orNewRequestID` (16
   `crypto/rand` bytes) because `StartAgentSessionRequest` carries no
   `client_request_id` — every production caller passes `""`
   (`service.StartAgentSession`, `lifecycle.provisionAndStart`,
   `lifecycle.freshStart`). The session id is therefore still Server-minted
   and never client-derivable; if a client-suppliable request id is ever added
   to Start, this derivation MUST be revisited (recorded in DL-371).
3. **Minting site.** `Hub.Start` computes `requestID := orNewRequestID(requestID)`
   once, then `freshID := h.newSessionID(requestID, req.GetContainerName())`.
   `newSessionID func(requestID, containerName string) string`
   is a field on `Hub` (`go/internal/runnerhub/hub.go`), defaulted in `NewHub`
   to `mintSessionID`, overridable through an exported
   `SetSessionIDMinter(func(requestID, containerName string) string)` seam in the style of
   `SetSessionBindingStore`. The setter exists for the `go/server` pgtest
   fixtures, which are out-of-package and assert on literal ids; production
   never calls it. `Hub.StartResume` never mints.
4. **Runner selection.** `agentHost.Start` gains a `freshSessionID string`
   parameter threaded from `dispatcher.execute` via
   `cmd.GetFreshSessionId()`. Selection order under `h.mu`:
   `req.GetResumeSessionId()` non-empty → resume path (unchanged, including
   the live-collision `errAlreadyRunning` guard); else `freshSessionID`
   non-empty → use it; else return a new sentinel `errMissingSessionID`.
   The sentinel maps in `dispatcher.errorResult` to
   `RUNNER_ERROR_CODE_FAILED_PRECONDITION` (Connect `FailedPrecondition`) — the
   code the enum reserves for an operator/deployment fault, which a Server
   older than the Runner is. It is NOT `INTERNAL`: an operator seeing this
   must read it as "Server and Runner versions disagree", not "the Runner
   crashed".
5. **`monotonicIDs` and the `newID` constructor parameter are deleted.** The
   Runner keeps no id minter at all. `NewSessionHost` drops its trailing
   `newID func() string` parameter; every test fixture that passed a counter
   closure passes the id through `Start` instead. Leaving a fallback minter in
   place would silently reintroduce the counter the first time an envelope
   arrives without the field, which is the exact failure class being closed
   (`rule://no-inert-gating` in spirit: no code path whose only job is keeping
   the old behaviour reachable).
6. **Runner refuses to trust a resume id AND a fresh id together.** If both
   are set the Runner takes `resume_session_id` and logs at Warn — mirroring
   the existing "resume body supplied without resume_session_id" skew log. The
   Server never sends both; a Runner that sees both is observing a Server bug
   and must not silently pick the fresh id (which would orphan the resume
   lineage).
7. **`TestReusedSessionIDConflictIsSwallowed` is kept and renamed (Matt's
   ruling).** The witness pins `promoteSession`'s swallow-and-fall-back on
   `ErrConflict`, and that posture is deliberately unchanged (no downstream
   handling). What changes is what reaching it MEANS: after T2 the Runner
   has no minter, so a fresh start can never carry an id the Server did not
   derive, and a conflict can only come from a Server-side collision — a
   bug. The test is renamed to say so, its `"sess-1"` fixture becomes a
   64-hex literal, and its comment drops the "become a real assertion when
   fixed" clause (this fix does not make the durable row agree with the
   cache; it makes the row unreachable from a restart). The regression pair
   that proves the restart class is closed is T2's
   `TestStartWithoutServerMintedIDFailsClosed` and T3's
   `TestStartCarriesFreshSessionIDAndResumeDoesNot`. Re-`enroll`-based
   conversions were rejected: a re-enroll reaps that Runner's rows
   (`Hub.enroll` → `DeleteSessionBindingsForRunner`, mirrored by
   `fakeBindingStore`), so a seeded row never reaches the conflict.
8. **The Server checks the Runner echoed its id, and reaps on mismatch.**
   `RecordAgentSession` still runs after the Runner answers, keyed by
   `resp.GetSessionId()`. `Hub.Start` compares `resp.GetSessionId()` to the
   derived id. Because the id is request-id-bound (Decision 2), a dedup echo
   from a retry or a router join compares EQUAL — the check never fires on a
   legitimate retry. It fires only on skew: a Runner that ignores tag 14 and
   answers from a minter of its own. On mismatch `Hub.Start` issues a
   bounded Stop for the id the Runner reported — the
   `abandonStartedSession` discipline: `context.WithoutCancel(ctx)` plus a
   30s bound (a `runnerhub` package var `mismatchStopTimeout`, mirroring
   `service.rollbackStopTimeout`, since the dispatch path has no deadline of
   its own) — then returns `connect.CodeInternal` naming BOTH ids, and never
   calls `promoteSession`. Without the Stop the Runner keeps a live agent
   that resolves to no account, and every later Start on that container is
   `AlreadyExists`. Consequence for tests: every fake Runner that answers a
   `start` command must echo `cmd.GetFreshSessionId()` (or the resume id)
   instead of a fixed literal — see T4's fixture list.

## Alternatives considered

### A. Runner-local random id (PR #1280)

Replace the counter with `"sess-" + hex(16 random bytes)` inside
`monotonicIDs`. Collision-free and a three-line diff. Lost because it keeps
the Runner as the minting authority for a key the Server persists, authorizes
and account-binds; every doc comment on that key already asserts the opposite.
It also leaves `NewSessionHost`'s `newID` injection point alive, so a later
"simplification" back to a counter passes review. Matt ruled for the
server-minted envelope.

### B. Per-enrollment nonce prefixing the counter

`sess-<enroll-nonce>-<n>`: keeps log-orderable ids and closes the restart
collision. Lost for the same authority reason as A, plus it needs the Runner
to learn the nonce at `Enroll` (a second proto touch for no gain over A's
random id).

### C. Handle `ErrConflict` in `promoteSession`

On conflict, delete or repoint the stale durable row. Explicitly ruled out by
Matt: it leaves the collision reachable and puts a correctness-critical
decision on an error path.

### D. A new `fresh_session_id` field on the public `StartAgentSessionRequest`

Rejected: a client-suppliable session id is a client-chosen storage key and
an authz-bypass vector (pick a live id, get its account). The internal
envelope is the only correct carrier, exactly as `resume_body` established.

### E. Drop the echo check; promote on whatever the Runner answers

After T2 the Runner has no minter, so the only way `resp.GetSessionId()`
differs from the Server's id is a skewed (older) Runner. Rejected: the check
is the only thing that turns that skew into a loud, named error instead of a
silently mis-keyed binding. With the request-id binding of Decision 2 the
check has no false positive on a retry, and Decision 8's bounded Stop means
a true positive is reaped when the Stop succeeds and named for an operator
when it does not.

### F. Per-call random session id, unbound from the request id

The first draft of this record: `Hub.Start` mints 16 random bytes per call.
Rejected by Matt (critique F1): a timeout retry or a concurrent router join
that reuses the request id gets the FIRST session id back from the Runner's
dedup, the second `Hub.Start` compares it to a NEW random id, and a live,
correctly bound session is reported as `CodeInternal`. Binding the id to the
request id makes the dedup echo compare equal by construction.

### G. Session id = request id verbatim

Simpler than a hash, same retry property. Rejected: it collapses two
identifiers with different lifetimes and audiences into one string (the
request id is a transient correlation key on the internal stream; the session
id is a durable public key), and a future client-suppliable request id on
Start would make the session id client-chosen — Alternative D's rejection
reason. The domain-separated hash keeps the coupling one-way and is the
construction `provisionDedupID` already uses.

## Global Constraints

- **Internal proto lane only.** `fresh_session_id` lives on
  `SessionsResponse` in `proto/compass/v1/runner.proto`, generated by
  `buf.gen.internal-go.yaml` into `go/internal/gen/compass/v1/runner.pb.go`.
  No change to `compass.proto`, no public gen, no TS gen. Regenerate with
  `moon run proto:gen`; CI's `proto:drift` and `proto:gen-fence` must stay
  green.
- **Tag 14, and only 14.** Do not reuse 12 (DL-065's abandoned
  `ResumeContext`); keep the existing comment on tag 13 that records why.
- **Id shape.** 64 lowercase hex chars:
  `hex(sha256(lenprefix("compass.session.v1") || lenprefix(request_id) || lenprefix(container_name)))`,
  the `provisionDedupID` construction. No prefix. The request id is
  `orNewRequestID(requestID)` computed ONCE in `Hub.Start` and used for both
  the envelope's `request_id` and the derivation — never two calls; the
  container name is `req.GetContainerName()`, the same value the relay
  routes on.
- **Retry idempotency is preserved.** Two `Hub.Start` calls with the same
  request id derive the same session id, so the Runner's request-id dedup
  (`dispatcher.handle`) and the router's in-flight join
  (`commandRouter.dispatch`) both echo an id that passes the Decision 8
  check. No test may rely on two same-request-id Starts yielding different
  session ids.
- **Deploy Server and Runner together; there is no version handshake.**
  `EnrollRequest` carries `runner_id`, `runtime_tier`, `egress_posture` and
  no version, so nothing gates a skew. Both skew directions are hard-down for
  FRESH starts and must be read as such: Runner newer than Server → every
  fresh start is `FailedPrecondition` ("fresh start carries no server-minted
  session id"); Server newer than Runner → the old Runner ignores tag 14,
  answers `sess-N`, the Server's echo check fires, Stops that session, and
  returns `CodeInternal` naming both ids ("runner answered Start with
  session "sess-1", want the server-minted <64-hex>"). RESUME starts keep
  working in both skews (the Runner takes `resume_session_id` first), which
  is the partial-availability floor. Roll Server and Runner in one release;
  do not roll one and wait.
- **Resume is untouched.** `resume_session_id` reuse as the live id, the
  `errAlreadyRunning` live-collision guard, the `filepath.Base` traversal
  guard, `COMPASS_RESUME_SESSION_FILE` export, and the Server's skip of
  `RecordAgentSession` on resume all stay exactly as they are.
- **No downstream conflict handling.** `promoteSession`'s `ErrConflict`
  fallback-to-RAM is not modified. `RecordAgentSession`'s `ErrConflict` on a
  reused id stays a hard error (it now indicates a Server bug, not a Runner
  restart).
- **No Runner-side minter survives.** `monotonicIDs` and the `newID`
  parameter on `NewSessionHost` are deleted in the same PR that adds the
  envelope field. A fresh start with an empty `fresh_session_id` fails closed
  with `RUNNER_ERROR_CODE_FAILED_PRECONDITION`; it never mints locally.
- **Test fixtures pass the id through `Start`.** Every `NewSessionHost` call
  site in `go/internal/runner/*_test.go` currently passes a counter closure;
  each becomes a `Start(ctx, req, "", "<literal id>")` call. Tests that
  assert a specific minted id assert on the literal they passed.
- **Go / Bun / Biome as pinned by the repo.** No new dependencies;
  `crypto/rand`, `crypto/sha256`, `encoding/binary` and `encoding/hex` are
  already imported in `runnerhub/commands.go`; `time` (for
  `mismatchStopTimeout`) and `errors` (for the `CodeInvalidArgument` guard's
  `errors.New`) are added there — the file imports `fmt` today but not
  `errors`.
- **Record governance.** This record lives in the governed
  `docs/designs/agent/` bucket (not `agents/`) and carries `Status: Draft`
  immediately after its H1 until the design PR merges, when Matt's approval
  flips it to `Status: Active` in the same PR that lands the `DL-371` row.
- **Two PRs.** (1) This record plus its `DL-371` ledger row as the last row
  of the `docs/designs/DECISIONS.md` § Storage table, after DL-370 and
  before the RIG-1570 R5 tag-12 blockquote on DL-065 (Matt's ruling: the
  session id is a storage key that happens to ride the envelope), subject
  `docs(agent): server-minted session ids (RIG-3696)`; (2) the
  implementation, T1–T4 as one PR on a single linear line
  (`rule://trunk-merge-queue`), subject
  `fix(runner): mint fresh session ids on the Server (RIG-3696)`, with
  `Ledger-impact: none` since the ledger row lands with the record.
  Commit identity per `rule://commit-conventions`.

## Plan

Five tasks. T1–T4 are the implementation PR, executed in order: T1 is the
wire; T2 the Runner; T3 the Server; T4 the tests and fixture conversions the
first three force. T5 is the record's ledger row (design PR) and the closing
of the two superseded PRs. T2 and T3 are separable commits but MUST land in
the same PR: after T2 alone a Runner refuses every fresh start until T3 sends
the field, and after T3 alone a Runner ignores the field and re-mints —
neither half is deployable on its own (`rule://no-inert-gating`).

### T1 — Envelope field (proto + gen)

Add `string fresh_session_id = 14;` to `message SessionsResponse` in
`proto/compass/v1/runner.proto`, directly after `ResumeBody resume_body = 13;`
with a comment in the file's house style that states: Server-minted and
DERIVED from this envelope's `request_id` (so a retry that reuses the request
id carries the same session id and the Runner's dedup echo matches), fresh
start only, ignored when `start.resume_session_id` is non-empty, internal
lane only, tag 14 is the next free tag after 13 (12 skipped per the DL-065
note on tag 13).

Extend the `RUNNER_ERROR_CODE_FAILED_PRECONDITION` enum comment's enumerated
cause list with the new cause: "a fresh Start whose envelope carries no
`fresh_session_id` — a Server older than this Runner (version skew), never a
Runner-local fault". The enum comment is the operator's index of what that
code means; an unlisted cause reads as one of the socket faults.

Regenerate `go/internal/gen/compass/v1/runner.pb.go` with
`moon run proto:gen`. No `gen-fence` edit: the fence in `proto/moon.yml`
matches message and service identifiers (`\bResumeBody\b`,
`\bRunnerService\b`, …), never scalar field names, and a string field on an
already-fenced message adds no new generated type.

Interfaces:

- Produces: `func (x *SessionsResponse) GetFreshSessionId() string` and the
  `FreshSessionId string` struct field on `compassv1internal.SessionsResponse`
  (`go/internal/gen/compass/v1`).
- Consumes: nothing.
- Proof: `moon run proto:drift` and `moon run proto:gen-fence` pass; `go
  build ./go/...` compiles unchanged behaviour (no consumer yet).

### T2 — Runner selects, never mints

In `go/internal/runner/dispatch.go`:

- `SessionHost.Start` becomes
  `Start(ctx context.Context, req *compassv1.StartAgentSessionRequest, resumeBody, freshSessionID string) (sessionID string, err error)`.
  Doc comment: `freshSessionID` is the Server-minted live id for a fresh start;
  empty on a resume; a fresh start with an empty value fails closed.
- `dispatcher.execute`, `SessionsResponse_Start` arm:
  `d.host.Start(ctx, c.Start, cmd.GetResumeBody().GetSessionBody(), cmd.GetFreshSessionId())`.
- `dispatcher.errorResult`: add
  `case errors.Is(err, errMissingSessionID): code = RUNNER_ERROR_CODE_FAILED_PRECONDITION`.
- Add `errMissingSessionID = errors.New("fresh start carries no server-minted session id")`
  to the existing `var (...)` block that declares `errAlreadyRunning` and
  `errSessionUnknown`.

In `go/internal/runner/host.go`:

- `agentHost.Start` selection (the block under `h.mu` that today calls
  `h.nextID()`):

  ```go
  sessionID := req.GetResumeSessionId()
  switch {
  case sessionID != "":
      if freshSessionID != "" {
          h.log.Warn("start carries both resume_session_id and fresh_session_id; taking the resume id", "container", name, "resume_session_id", sessionID)
      }
      if _, live := h.sessions[sessionID]; live {
          h.mu.Unlock()
          return "", errAlreadyRunning
      }
  case freshSessionID != "":
      sessionID = freshSessionID
  default:
      h.mu.Unlock()
      return "", errMissingSessionID
  }
  ```

  Keep the existing comment explaining why a resume reuses the logical id;
  replace "A fresh start mints a new id" with "A fresh start uses the id the
  Server minted on the envelope (RIG-3696); the Runner never mints".
- Delete `monotonicIDs`, the `nextID` field on `agentHost`, and the trailing
  `newID func() string` parameter of `NewSessionHost` (new signature:
  `NewSessionHost(link *ServerLink, rt *runtime.AgentRuntime, registry *runtime.AgentRegistry, engine runtime.WorkloadRuntime, specs SpecBuilder, cfg AgentHostConfig, log *slog.Logger) SessionHost`).
  `strconv` is imported by `host.go` only for `monotonicIDs`; drop the import
  with it.
- `go/internal/runner/run.go`: update the one production `NewSessionHost`
  call (drop the trailing `nil`).

Interfaces:

- Consumes: `SessionsResponse.GetFreshSessionId()` (T1).
- Produces: the new `SessionHost.Start` signature; `errMissingSessionID`;
  `NewSessionHost` without `newID`.
- Proof (focused, `go test ./go/internal/runner/ -run 'TestStart|TestReload|TestDispatch|TestHandle'`
  plus the new tests in T4): a fresh `Start` with a fresh id returns exactly
  that id; a resume `Start` returns `resume_session_id` and ignores a fresh id
  (with the Warn line); a fresh `Start` with neither fails with
  `errMissingSessionID` and the dispatcher maps it to
  `RUNNER_ERROR_CODE_FAILED_PRECONDITION`; `Reload` still reuses the id.

### T3 — Server mints on fresh start

In `go/internal/runnerhub/hub.go`:

- Add `newSessionID func(requestID, containerName string) string` to `Hub`
  (comment: derives the live id for a fresh start from the envelope request
  id and the target container, RIG-3696; defaulted to `mintSessionID`; read
  under mu).
- `NewHub`: `newSessionID: mintSessionID`.
- Add `func (h *Hub) SetSessionIDMinter(mint func(requestID, containerName string) string)`
  beside the other `Set*` seams (lock, assign, unlock; a nil argument
  restores `mintSessionID`).

In `go/internal/runnerhub/commands.go`:

- Add `var mismatchStopTimeout = 30 * time.Second` with the
  `rollbackStopTimeout` comment shape (the dispatch path has no deadline; a
  package var only so a test can shorten it).
- Add `func mintSessionID(requestID, containerName string) string`: SHA-256
  over the length-prefixed fields `"compass.session.v1"`, `requestID`,
  `containerName` in that order, exactly the
  loop body of `provisionDedupID` (8-byte big-endian length, then bytes),
  `hex.EncodeToString(h.Sum(nil))`. Comment: this is the ONLY minting site
  for a fresh live session id; the Runner never mints; the derivation binds
  the session id to the request id so a same-request-id retry or router join
  returns the same session id (the envelope's OQ6 idempotency), and the
  domain separator keeps a request id from ever being a valid session id in
  another derivation. Extract the shared length-prefixed hashing into a
  small unexported helper `hashFields(domain string, fields ...string) string`
  used by both `provisionDedupID` and `mintSessionID` — one construction,
  two callers, no second convention. `mintSessionID(requestID, containerName string)`
  hashes the fields in that order under domain `"compass.session.v1"`; the
  container name is the second input (Matt's ruling) so the same request
  id replayed against a DIFFERENT container can never derive the same
  session id — the container is what the id keys on the Runner side.
- `Hub.Start`:

  ```go
  func (h *Hub) Start(ctx context.Context, requestID string, req *compassv1.StartAgentSessionRequest) (*compassv1.StartAgentSessionResponse, error) {
      if req.GetResumeSessionId() != "" {
          return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("runnerhub: Start with resume_session_id; use StartResume"))
      }
      requestID = orNewRequestID(requestID) // once: envelope key AND derivation input
      h.mu.Lock()
      mint := h.newSessionID
      h.mu.Unlock()
      freshID := mint(requestID, req.GetContainerName())
      result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
          RequestId:      requestID,
          Command:        &compassv1internal.SessionsResponse_Start{Start: req},
          FreshSessionId: freshID,
      })
      if err != nil {
          return nil, err
      }
      resp := result.GetStart()
      if resp == nil {
          // A start result with no start variant is a wire-contract skew; there
          // is no session id to stop, so fail loud without a Stop.
          return nil, connect.NewError(connect.CodeInternal, errors.New("runner answered Start with no start result"))
      }
      if got := resp.GetSessionId(); got != freshID {
          // Skew: a Runner that ignored fresh_session_id and minted its own. The
          // agent is live under an id no account will ever resolve; reap it
          // (bounded, abandonStartedSession discipline) before failing loud.
          stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mismatchStopTimeout)
          defer cancel()
          if _, stopErr := h.Stop(stopCtx, "", &compassv1.StopAgentSessionRequest{SessionId: got}); stopErr != nil {
              // Remedy: an operator stops runner_session_id (or removes the
              // container) by hand; both ids and the container are named here.
              h.log.Error("stopping mis-keyed session after fresh_session_id mismatch failed; session is stranded live",
                  "container", req.GetContainerName(), "runner_session_id", got, "server_session_id", freshID, "error", stopErr)
          }
          return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("runner answered Start with session %q, want the server-minted %q (Server/Runner version skew; stop of %q attempted)", got, freshID, got))
      }
      h.promoteSession(ctx, req.GetContainerName(), freshID)
      return resp, nil
  }
  ```

  `Hub.Stop` on the mismatch path relays a Stop for the Runner-reported id
  and calls `unbindSession(got)`, which is a no-op for an id that was never
  promoted — correct and idempotent. The `CodeInvalidArgument` guard is new:
  today `service.StartAgentSession` already demuxes resume to
  `startResumeSession`, and `lifecycle.go`'s two `hub.Start` callers build
  the request with no `ResumeSessionId`; the guard makes the "Start never
  carries a resume id" invariant local to the hub instead of an assumption
  about its callers.
- `Hub.StartResume` (`resume_start.go`): unchanged — it sets no
  `FreshSessionId`. Add one sentence to its comment: "It never sets
  `fresh_session_id`; the resume reuses the authorized logical id (RIG-3696)."

`go/server/service.go` and `go/server/lifecycle.go` need no code change: they
read `resp.GetSessionId()` and that is now the Server's own id; on a
`Hub.Start` error the hub has already attempted the Stop of any mis-keyed
session, so `service.StartAgentSession`'s bare `return nil, err` and
`lifecycle.provisionAndStart`'s `rollbackSpawn(ctx, container, "")` add no
second strand on the happy Stop. If the Stop itself fails, the agent IS
stranded live; the remedy is the Error log line above, which names the
container and both ids so an operator can `StopAgentSession` the
Runner-reported id (or `RemoveAgentWorkspace` the container) by hand — the
same posture `abandonStartedSession` takes when its Stop fails.
Update the comment on `StartAgentSession` ("A fresh start mints a
new id and records below") to say the hub derives it from the relay request
id.

Interfaces:

- Consumes: `SessionsResponse.FreshSessionId` (T1).
- Produces: `Hub.SetSessionIDMinter(func(requestID, containerName string) string)`;
  `mintSessionID(requestID, containerName string) string`; `hashFields`;
  `mismatchStopTimeout`; `Hub.Start` now rejects a `resume_session_id` with
  `CodeInvalidArgument` and a mismatched Runner echo with a bounded Stop +
  `CodeInternal`.
- Proof (focused, `go test ./go/internal/runnerhub/ -run 'TestStart|TestSessionIDMint|TestReusedSessionID'`):
  a fresh `Start` puts a non-empty 64-hex `fresh_session_id` equal to
  `mintSessionID(cmd.GetRequestId(), cmd.GetStart().GetContainerName())` on
  the pushed envelope and returns it;
  two `Start`s with the same explicit request id (the second joined to the
  first in-flight call, or dispatched after it settled with the fake echoing
  the first id) both succeed with the same session id, the router pushed ONE
  command and served both callers the same result, and `accountForSession`
  resolves that single binding;
  `StartResume` pushes an empty `fresh_session_id`; a Runner echoing a
  different id gets a Stop for that id, `CodeInternal`, and no binding for
  either id.

### T4 — Focused regression tests and fixture conversions

New tests (each defends one observable contract; none asserts wiring):

- `go/internal/runner/host_test.go`
  - `TestStartUsesServerMintedIDForFreshStart`: `Start(ctx, {ContainerName}, "", "abc123")`
    returns `"abc123"`, and `Status` reports that id.
  - `TestStartWithoutServerMintedIDFailsClosed`: fresh `Start` with `""`
    returns `errMissingSessionID`; no `ExecStreaming` happened on the engine
    (the agent was never launched); the container stays `Start`able
    afterwards (a retry with an id succeeds).
  - `TestStartResumeIgnoresServerMintedID`: `Start(ctx, {ContainerName, ResumeSessionId: "logical-1"}, "body", "fresh-9")`
    returns `"logical-1"`, materializes `.compass/resume/logical-1.jsonl`
    (a recording-exec write with stdin `"body"`, as
    `TestStartWithResumeBodyMaterializesSessionFile` checks), and `Status`
    lists `logical-1` only — never `fresh-9`. Do not assert the Warn text;
    the log line is diagnostics, not contract.
- `go/internal/runner/dispatch_test.go`
  - `TestStartMissingSessionIDMapsToFailedPrecondition`: a
    `fakeSessionHost` returning `errMissingSessionID` yields a `RunnerError`
    with `RUNNER_ERROR_CODE_FAILED_PRECONDITION`.
  - Extend `fakeSessionHost.Start` to record the `freshSessionID` it was
    handed, and assert in the existing `TestHandleDedupExecutesHostOnce`
    path (or a sibling) that the dispatcher threads `cmd.FreshSessionId`
    through unchanged.
- `go/internal/runnerhub/commands_test.go`
  - `TestSessionIDMintIsDerivedFromRequestIDAndContainer`:
    `mintSessionID("r", "c")` is 64 hex chars, equals itself on a second
    call, differs from `mintSessionID("s", "c")` and from
    `mintSessionID("r", "d")`, and differs from `provisionDedupID("r", …)`
    for overlapping input (domain separation).
  - `TestStartCarriesFreshSessionIDAndResumeDoesNot`: attach a router that
    captures `cmd.GetRequestId()`/`cmd.GetFreshSessionId()` and echoes the
    fresh id; fresh `Start` → captured fresh id equals
    `mintSessionID(capturedRequestID)` and equals the returned `session_id`;
    `StartResume` → captured fresh id is empty and the returned id is the
    resume id.
  - `TestStartRetryWithSameRequestIDReturnsSameSession`: two `Start` calls
    with request id `"req-1"` — the first held in flight behind a gate so the
    second joins it in `commandRouter.dispatch`, then released; both return
    the same session id, the router saw ONE push, and `accountForSession`
    resolves that id once. This is the F1 hole made a test.
  - `TestStartMismatchStopsRunnerSessionAndFailsInternal`: router echoes
    `"other"` for `start` and records a `stop`; `Start` is `CodeInternal`
    with both ids in the message; the router saw a `stop` for `"other"`;
    `accountForSession("other")` and `accountForSession(<derived>)` are both
    `ok=false`. Shorten `mismatchStopTimeout` in a sibling case where the
    router never answers the Stop and assert `Start` still returns within
    the bound.
  - `TestStartRejectsResumeIDOnFreshPath`: `Start` with `ResumeSessionId`
    set is `CodeInvalidArgument` and pushes nothing on the router.
- `go/internal/runnerhub/binding_cache_test.go`
  - KEEP `TestReusedSessionIDConflictIsSwallowed`, renamed
    `TestReusedSessionIDConflictIsAServerBug` (Matt's ruling), with its
    `"sess-1"` fixture replaced by a 64-hex literal (e.g. `strings.Repeat("ab", 32)`)
    so the fixture matches what the Server now mints. Its doc comment is
    rewritten: the Runner can no longer re-mint over a surviving row (T2
    deleted its minter), so reaching this path now means a Server-side id
    collision — a bug, not a restart — and the test pins that
    `promoteSession` STILL swallows the `ErrConflict` and falls back to RAM
    (Matt: no downstream handling), leaving the durable row on the stale
    account. The assertions are unchanged; the failure message's "if this
    now agrees with the cache … become a real assertion" sentence is
    dropped because the fix chosen does not make them agree. Also update the
    comment inside `fakeBindingStore.RecordSessionBinding` (same file,
    `binding_cache_test.go`) that says "a Runner restart re-mints "sess-1",
    so id reuse is routine" to say id reuse is now a Server bug (DL-371) and
    the fake still refuses it so such a bug fails loud.
- End-to-end proof over the real seam (a REAL `agentHost` behind a REAL
  `Hub` through `runnerloop.RunSessionsLoop`): `integration_pgtest_test.go`
  (`go/internal/runnerhub`) and `lifecycle_e2e_pgtest_test.go`
  (`go/server`) are the only tests where T3's envelope id must reach T2's
  selection unfaked. Add one assertion to each existing fresh-start path:
  the id `hub.Start` returns is 64 hex chars and `host.Status` (or the
  relayed `GetAgentStatus`) reports the same id. These two are named T4
  proof, not incidental compile fixes.

Fixture conversions (mechanical; no new assertions):

- Every `NewSessionHost(...)` call site drops its trailing `newID`/`nil`
  argument. Enumerate with grep `NewSessionHost(` across `go/`, not
  `go/internal/runner` alone; today that is `go/internal/runner/run.go`
  (production), `host_test.go` ×6, `host_gateway_test.go`,
  `host_vsock_gateway_test.go`, `e2e_transport_test.go`,
  `e2e_vsock_gateway_microvm_test.go`, `secrets_refresh_test.go`,
  `config_refresh_test.go`, `config_delivery_e2e_test.go`, and the two
  out-of-package fixtures `go/internal/runnerhub/integration_pgtest_test.go`
  and `go/server/lifecycle_e2e_pgtest_test.go`.
- Every `host.Start(ctx, req, body)` call becomes
  `Start(ctx, req, body, id)` with a literal `id` for a fresh start and `""`
  for a resume. Enumerate with grep `\.Start\(ctx` in
  `go/internal/runner/*_test.go`; today that adds `agent_exec_test.go`,
  `e2e_retire_test.go`, and `host_concurrency_test.go` (three sites) to the
  files above. Tests that read the returned id keep doing so; tests that
  relied on the counter producing `sess-1`, `sess-2` for two containers pass
  two distinct literals.
- `fakeSessionHost.Start` (declared in `dispatch_test.go`, shared by
  `run_seam_test.go`) and the separate `fakeSessionHost` in
  `runnerhub/runner_dispatch_concurrency_test.go`: add the fourth parameter.
- Every fake Runner that answers a `start` command with a literal id must
  echo `cmd.GetStart().GetResumeSessionId()` when non-empty, else
  `cmd.GetFreshSessionId()` — mirroring `agentHost.Start`'s selection so the
  `Hub.Start` echo check passes. Enumerate with grep
  `StartAgentSessionResponse{SessionId:` across `go/`; today:
  `runnerhub/commands_test.go` (`"sess-ok"` ×2, in
  `TestStartRelayReturnsSessionIdOnSuccess` and the responder below it),
  `runnerhub/seam_test.go` (`"sess-wire"`, once), `runnerhub/relay_comms_test.go`
  (`"sess-live"` ×2, whose `accountForSession("sess-live")` assertions then
  read the captured envelope id instead), `runnerhub/concurrent_dispatch_test.go`
  (`"sess-" + cmd.GetRequestId()` — the `want := "sess-" + id` assertion
  becomes "equals the id captured from its own command", still proving no
  interleaving), and `go/server/service_placement_pgtest_test.go`
  `recordingRunner` (both the `answer` arm and the `nextStartID` serve-loop
  arm). EXCLUDED on purpose: `runnerhub/router_test.go`'s `startResult`
  helper (`"sess-42"`, `"sess-a"`, …) — those cases drive `commandRouter`
  directly and never pass through `Hub.Start`, so the echo check does not
  run and the literals stay.
- `go/server/service_placement_pgtest_test.go`: delete the `startIDs` FIFO
  and `setStartIDs`/`nextStartID`. `placementFixture` calls
  `hub.SetSessionIDMinter` with a FIFO minter that yields `fakeSessionID`
  by default and the literal ids a test previously passed to `setStartIDs`
  (`lifecycle_pgtest_test.go`, `forge_notify_e2e_pgtest_test.go`,
  `trace_continuity_e2e_pgtest_test.go`). `service_resume_pgtest_test.go`'s
  `setStartIDs("live-resume-1", ...)` is dropped outright: those are resume
  Starts, and the fake now echoes the logical resume id as the real Runner
  does.
- `apps/ui/src/store.live.test.ts`: the comment citing `monotonicIDs` and
  `host.go:322-331` is stale after T2; reword to "a server-minted 64-hex id
  derived from the relay request id (`mintSessionID`,
  `go/internal/runnerhub/commands.go`)". The literal `"sess-7"` may stay —
  the UI treats the id as opaque.

Interfaces:

- Consumes: T2's `SessionHost.Start` signature and `errMissingSessionID`;
  T3's `SetSessionIDMinter`.
- Produces: the tests named above; a `recordingRunner` that echoes the
  envelope id.
- Proof: `go test ./go/internal/runner/... ./go/internal/runnerhub/...` and
  the `pgtest`-tagged `go/server` suite pass; `go vet ./go/...` clean.

### T5 — Ledger, record hygiene, and superseded PRs

- Append `DL-371` as the last row of the `docs/designs/DECISIONS.md`
  § Storage table, immediately after `DL-370` and BEFORE the
  `> Note (2026-07-31, RIG-1570 R5)` blockquote on DL-065's retired tag 12
  (Matt's ruling: beside DL-065's lineage; a row cannot follow prose inside
  a Markdown table):
  "Fresh live session ids are SERVER-MINTED and DERIVED from the Sessions
  envelope's Server-generated `request_id` and the target `container_name`
  (`hex(sha256(domain "compass.session.v1" ‖ request_id ‖ container_name))`,
  64 hex, no prefix — the `provisionDedupID` construction) so a
  same-request-id retry
  or router join yields the same session id and the envelope's OQ6
  idempotency holds; carried Server→Runner on
  `SessionsResponse.fresh_session_id = 14` (internal lane, top-level sibling
  of `resume_body = 13`; 12 stays skipped). The Runner selects
  `resume_session_id` first, else the envelope id, else fails closed
  `FAILED_PRECONDITION`, and keeps NO minter (`monotonicIDs` deleted). The
  Server checks the Runner echoed its id and on mismatch stops the
  Runner-reported session (bounded) before failing `CodeInternal`. Resume
  keeps reusing the authorized logical id; `promoteSession`'s `ErrConflict`
  fallback is NOT a recovery path — its RIG-3108 review-F1 witness test is
  KEPT and renamed to say the reused id is now a Server bug, with a 64-hex
  fixture id (the Runner can no longer re-mint over a surviving row, but the
  Server's swallow-and-fall-back posture on a conflict is unchanged).
  Server and Runner deploy together; no version handshake exists. If a
  client-suppliable request id is ever added to Start, the derivation must
  be revisited (Alternative D)". Status cell: `Active (Matt, YYYY-MM-DD)`
  with the date of Matt's approval of the design PR, written at
  implement-time by whoever opens that PR; Record cell:
  `[runner session-id allocation §Decisions](agent/runner-session-id-allocation/design.md#decisions)`.
  This
  row ships in the DESIGN PR, same diff as this record.
- Close PR #1280 and PR #1283 with a one-line comment pointing at the design
  PR and the implementation PR once the latter is open; update RIG-3696's
  description to name the implementation PR.
- No code comment cites a flipped DL row (none is flipped), so no sweep.

Interfaces:

- Consumes: the design PR number (row), the implementation PR number
  (closing comments).
- Produces: the ledger row; closed superseded PRs.
- Proof: `tools/design-ledger-gate` passes on the design PR.

## Tasks

- [ ] T1 — `fresh_session_id = 14` on `SessionsResponse` (comment: derived
      from `request_id`); `FAILED_PRECONDITION` enum comment gains the
      no-envelope-id cause; regen `go/internal/gen`; `proto:drift` +
      `proto:gen-fence` green.
- [ ] T2 — Runner: `SessionHost.Start` takes `freshSessionID`; selection
      resume → fresh → `errMissingSessionID` (`FAILED_PRECONDITION`);
      `monotonicIDs` and `NewSessionHost`'s `newID` deleted; `run.go` updated.
- [ ] T3 — Server: `hashFields` + `mintSessionID(requestID, containerName)` +
      `Hub.newSessionID` + `SetSessionIDMinter` + `mismatchStopTimeout`;
      `Hub.Start` computes the request id once, derives the session id,
      rejects a resume id, checks the Runner echo and Stops the mis-keyed
      session on mismatch; `StartResume` untouched.
- [ ] T4 — Regression tests (runner host ×3, dispatcher ×2, runnerhub ×5
      incl. same-request-id retry and mismatch-Stop, two real-seam e2e
      assertions); witness test kept and renamed; the complete fixture list
      above.
- [ ] T5 — `DL-371` under § Storage beside DL-065's tag-12 note; close PR
      #1280 and PR #1283; RIG-3696 description updated.

## Resolved decisions

Rulings folded from the design-critic pass (F1–F7) and Matt's review; the
record above is the decided outcome.

1. **Session id bound to the request id AND container name** (critique F1
   → Matt): derived `hex(sha256(domain ‖ request_id ‖ container_name))`,
   never a per-call random value, so the envelope's request-id idempotency
   survives the echo check and a request id reused across containers cannot
   derive one id for two sessions. Decision 2, Alternatives F/G, T3.
2. **Bounded Stop on echo mismatch** (F2): the Runner-reported session is
   stopped under `mismatchStopTimeout` before `CodeInternal`. Decision 8, T3.
3. **Deploy together; both skews named** (F3): Global Constraints.
4. **Witness test kept and renamed with a 64-hex fixture** (F4 → Matt):
   Decision 7, T4, T5.
5. **DL-371 under § Storage beside DL-065** (F5 → Matt): Global
   Constraints, T5.
6. **Complete fixture and e2e proof list** (F6): T4.
7. **`FAILED_PRECONDITION` enum comment extended** (F7): T1. The code
   escapes to the public `StartAgentSession` caller and folds into
   `wakeOutcomeFailed` on the wake path; both are the intended
   operator-visible surface for a deployment skew.
8. **`sess-` prefix dropped; `FAILED_PRECONDITION` for a missing envelope
   id; `Hub.Start` rejects a resume id** — the first draft's three
   non-load-bearing assumptions, unchallenged by the critique, stand as
   written in Decisions 2, 4 and T3.
9. **Governance** (updated critique): record relocated from
   `docs/designs/agents/` to the governed `docs/designs/agent/` bucket;
   `Status: Draft` after the H1; `DL-371` sits inside the § Storage table
   after DL-370 (before the tag-12 blockquote) with its Record cell pointing
   at `agent/runner-session-id-allocation/design.md#decisions` and its date
   written at implement-time. Global Constraints, T5.
10. **`Status: Draft` on this record and `Active` on the DL-371 row are
    left exactly as written** (Matt's ruling): the contradiction is
    transitional and intentional — the design PR carries `Draft` while it
    is reviewed, and the ledger row is authored `Active` so the merge that
    freezes the record needs no second edit to the row.
11. **Review lows folded**: nil `start` result guarded before the mismatch
    Stop (T3); the "strands nothing" claim softened and the Stop-failed
    remedy documented (Alternative E, T3); `seam_test.go` listed once and
    `router_test.go`'s `startResult` literals excluded with the reason
    (T4); Alternative B's "no gain over A's random id" typo fixed.
