# Compass forge write path

Status: Active
Tracker: RIG-2170

## Problem / Intent

Agents cannot yet write to a forge through Compass: `AgentGateway.Forge` exists
only as the generated Unimplemented stub, so the entire DL-048 ownership story —
the Server as the sole write chokepoint, stamping unforgeable attribution — has
no production caller. This record designs the complete forge WRITE path:
the production `AgentGateway.Forge` (Runner) + `RunnerService.RelayForgeCall`
(Server) handlers wiring `forge.StampOwner` as the first production stamp
caller; GitHub Provider write methods on the real client; a NEW
reviews/review-comments surface (proto arm + Provider method + GitHub review-API
mapping); and a Linear provider (read + write), extending DL-051's
"Linear issues-only" scope.

### Grounded current state (verified in this workspace, main tip `5089e1de`)

- **`AgentGateway.Forge` is a stub.**
  `go/gen/compass/v1/compassv1internalconnect/agent_gateway.connect.go:375-377`:
  `func (UnimplementedAgentGatewayHandler) Forge(…) { return nil,
  connect.NewError(connect.CodeUnimplemented, errors.New("compass.v1.AgentGateway.Forge is not implemented")) }`.
  The runner-side `Gateway` (`go/internal/runner/gateway/gateway.go:131`
  "`compassv1internalconnect.UnimplementedAgentGatewayHandler`" embedded) implements
  `Comms` (gateway.go:321), `Lifecycle` (lifecycle.go:54), `Control`, `Publish`,
  `PostConversationFrame` — **no `Forge` method** (grep over
  `go/internal/runner/gateway` for `func (g *Gateway)` returns no Forge).
- **`RelayForgeCall` is defined but unhandled.** The RPC + gen exist
  (`runner.connect.go:161` + `:430` `RelayForgeCall(context.Context,
  *connect.Request[v1.RelayForgeCallRequest]) (*connect.Response[v1.RelayForgeCallResponse], error)`,
  mount at `:529-534`; `runner.pb.go:1649` `type RelayForgeCallRequest struct`
  with `SessionId` + `Call *ForgeCallRequest`), but
  `go/internal/runnerhub/handler.go` routes only `RelayCommsCall` (:166),
  `RelayLifecycleCall` (:184), `RelayBoardCall` (:203) — RelayForgeCall falls
  through to the generated Unimplemented stub (`runner.connect.go:602-616`
  pattern).
- **`forge.StampOwner` is built, tested, and caller-less in production.**
  `go/internal/forge/owner.go:110`: `func StampOwner(body string, author Author,
  bodyLimit int) (string, error)` — idempotent, strip-then-stamp ("Strip any
  pre-existing/forged header BEFORE reserving and writing our own", owner.go:121-122),
  header bytes reserved against `bodyLimit` (owner.go:125-127
  `if bodyLimit > 0 && len(clean) > bodyLimit-len(hdr) { return "", ErrBodyTooLarge }`).
- **`Provider` already declares the write methods; GitHub does not implement
  them.** `go/internal/forge/provider.go:200-212` declares `Name`, `CreateIssue`,
  `CommentOnIssue`, `GetIssue`, `ListIssues`, `CreatePullRequest`,
  `CommentOnPullRequest`, `GetPullRequest`, `Checks` — **no review method**.
  `go/internal/forge/github.go` implements only `NewGitHub` (:102),
  `ListIssuesPage` (:140), `ListIssues` (:229) + rate-budget helpers; the write
  half exists only on `FakeProvider` (`fake.go:99-160`). `provider.go:216`
  already ships the unsupported-op mechanism: `var ErrUnsupported =
  errors.New("forge: operation unsupported by this provider")` ("#995 Decision 3",
  provider.go:214-215).
- **Proto carrier.** `proto/compass/v1/agent_gateway.proto:214-227`
  `ForgeCallRequest.call` oneof holds `create_issue`(2) … `get_pull_request`(8),
  `subscribe`(9)/`unsubscribe`(10) — **no `submit_review`**. Result arms
  (:235-247) retype to canonical `compass.v1.Issue`/`PullRequest` + `CommentRef`
  acks (DL-069/DL-092). `ForgeCallError` (:252-256) carries
  `code`/`message`/`retry_after_ms` — in-band, never a transport teardown.
  `agent_gateway.proto:260-263`: "`repo` is `<owner>/<name>` on GitHub and the
  project key on Linear, REQUIRED on every call … Request addressing is
  single-forge for a GitHub-first v1 (repo unambiguous); an optional ForgeRef is
  a named additive follow-up."
- **Canonical review types already exist on the wire (read side).**
  `proto/compass/v1/compass.proto:884-894`: `message Review { string author = 1;
  bool is_bot = 2; string verdict = 3; string body = 4; }` and `ReviewThread`
  (path/resolved/comments), carried on `PullRequest.reviews = 14` /
  `.threads = 15` (compass.proto:843-844). `forge.proto:48-54` `CommentRef`
  (url, comment_id, body, forge_account, agent) is the ruled write-ack shape
  (amendment §Resolved decisions OQ-B).
- **The sibling relay pattern to mirror.** `go/internal/runnerhub/relay_board.go:39-41`
  `type BoardCaller interface { SetIssueStateAsAccount(ctx, caller store.AccountID, req …) }`;
  guard order at :62-76 (nil-caller → CodeUnavailable before resolution;
  unbound session → CodeNotFound "never a stale account, never the bootstrap
  admin"); tool errors in-band (:81-91). Wiring:
  `go/server/sinks.go:89-92` `wireHubServiceCycles`:
  `hub.SetLifecycleCaller(newLifecycleService(st, hub));
  hub.SetBoardCaller(newBoardService(st, issueBrd))`. Runner side:
  `go/internal/runner/gateway/lifecycle.go:54-82` (resolve container→session,
  fail closed `CodePermissionDenied` when unbound, forward, nil-result →
  CodeInternal).
- **Server-held credential plumbing exists for GitHub reads.**
  `go/server/serve.go:761-762`: `tok := newForgeTokenSource(resolver, fc.SecretName);
  client := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: tok})` under
  the declared `server_only` secret name (serve.go:122-124 "the VALUE never
  crosses config or a flag"; default `GITHUB_FORGE_TOKEN`, serve.go:132).
- **No Linear code exists.** grep for `Linear|linear` over `go/internal/forge` +
  `go/internal/ingest`: zero matches. The enum slot exists:
  `go/internal/store/issues.go:35` `ForgeProviderLinear ForgeProvider = 4` and
  `compass.proto:781` `FORGE_PROVIDER_LINEAR = 4`.
- **No DL-055 ownership-index table exists.**
  `go/internal/store/migrations/0001_init.sql` (the only migration) creates
  `forge_repo_subscriptions` (:534), `agent_forge_subscriptions` (:547),
  `forge_artifact_cursors` (:567), `forge_list_cursors` (:587) — no
  authored-artifact row table.

## Global Constraints

- **Read/write split (driver-decided; do not re-litigate).** RIG-1728 owns READ:
  the GitHub read Provider (`ListIssuesPage`/`ListIssues`/`GetIssue`/
  `GetPullRequest`/`Checks`), the `StampOwner` primitive itself, ingestion
  translation, board projection. RIG-2170 (this record) owns the ENTIRE WRITE
  path and the handler pair; the handler's read arms are answered from the
  projection/store per the amendment's OQ-A ruling (amendment design.md:474-476
  "the `Forge` read ops survive, answered from the projection/store") for
  TRACKED artifacts, composing a live forge fetch + `TranslateIssue` only for
  an artifact the store does not track (amendment design.md:479-482), consuming
  RIG-1728's projection — never redoing it.
- **Toolchain:** Go `go 1.25.0` floor per `go/go.mod:15` (`go 1.25.0`), built
  under toolchain go1.26.6; `MOON_TOOLCHAIN_FORCE_GLOBALS=true` for all moon
  invocations.
- **DL reconciliation list (frozen; this record composes with, never
  contradicts):** DL-048 (Server is ownership layer), DL-049 (`ForgeCall*`
  sibling family, one envelope both hops), DL-050 (`StampOwner` sole chokepoint;
  parsed header = untrusted display), DL-051 (forge adapter behind swappable
  `Provider` — this record AMENDS its "Linear issues-only" scope, see §Ledger
  delta), DL-052 (Server-only write creds as `server_only` declared secrets;
  agent keeps push-scoped git cred), DL-055 (row per authored artifact, written
  with the stamp), DL-069/DL-092 (canonical `compass.v1` types only on the wire;
  no forge-shaped wire type), DL-129 (board-state writes are `BoardCall*`;
  forge-artifact writes are `ForgeCall*` — distinct families).
- **Canonical-types-only-on-wire:** every new result arm is a canonical
  `compass.v1` type or a `CommentRef`-style reference in `forge.proto` — never a
  raw forge shape (DL-069/DL-092).
- **Server-holds-write-creds (three declared secrets; F1 ruling 2026-08-17):**
  every Provider write executes under a Server-held `server_only` declared
  secret (DL-052). GitHub holds TWO: the AUTHOR credential
  (`GITHUB_FORGE_TOKEN`, serve.go:761-762) for every write arm except
  `submit_review`, and a DISTINCT REVIEWER identity
  (`GITHUB_FORGE_REVIEWER_TOKEN`) for the `submit_review` arm — reviewer ≠
  author, so APPROVE/REQUEST_CHANGES/COMMENT are all usable on Compass-
  authored PRs. The Linear token is a third such secret. All provisioned via
  the sanctioned declared-secret path (IaC, rule://no-human-clicks). No
  credential value ever crosses a flag or the wire.
- **Test strategy:** differential-oracle pyramid per DL-174 (hermetic in-memory
  reference — `FakeProvider` + `httptest`-backed clients — in the default gate;
  `pgtest` suites for real-Postgres contracts) with pgtest running INLINE in the
  one existing CI job per DL-175.
- **Error split (uniform across every task):** a tool-level failure is the
  in-band `ForgeCallError` variant (`code` = Connect status token, `message`,
  `retry_after_ms`); only a resolution miss / unmounted seam is a Connect error
  (`relay_board.go:48-57` guard-order convention).
- **Body handling:** write requests arrive WITHOUT the owner header
  (`agent_gateway.proto:265` "The write requests carry the body WITHOUT the
  owner header — the Server stamps it"); the Service stamps via `StampOwner`
  before any Provider sees the body; read arms return header-stripped bodies
  (DL-050).
- **Create idempotency (F3 ruling 2026-08-17):** `create_issue`/
  `create_pull_request` are idempotent under a caller-minted
  `client_request_id` whole-chain key mirroring the spawn precedent
  (agent_gateway.proto:172: `string client_request_id = 4; // whole-chain
  idempotency key (handler-level join + Provision dedup)`) — a retried create
  with the same key returns the ORIGINAL artifact, never a duplicate. The
  correlation-only `call_id` is NOT the key (agent_gateway.proto:209-210:
  "`call_id` is the agent-minted correlation id (the SDK toolCallId)").

## Approach

The write path composes five existing seams — it invents no new architecture.
Every piece below extends a pattern already frozen and built in the tree.

### 1. The `ForgeCaller` seam + `RelayForgeCall` Server leg (the DL-050 chokepoint)

Mirror the built `BoardCaller` pattern exactly (`relay_board.go:39-41`,
`sinks.go:89-92`):

- **`runnerhub/relay_forge.go`** — a new `ForgeCaller` interface + `Hub.SetForgeCaller`
  - `Hub.RelayForgeCall` with the identical fail-closed guard order
  (`relay_board.go:50-57`): (1) no caller wired → `CodeUnavailable` before
  resolution; (2) `accountForSession` miss → `CodeNotFound`, never a stale
  account, never bootstrap admin; (3) delegate under the RESOLVED
  `store.AccountID`. Tool-level failures return in-band as the
  `ForgeCallResult_Error` variant (`boardCallError` twin: `code =
  connect.CodeOf(err).String()`).
- **One deliberate deviation from `executeBoardCall`:** board dispatch is a
  one-arm switch in the hub (`relay_board.go:106-120`); forge has eleven arms
  plus stamping, provider selection, and store access, so the oneof dispatch
  moves INTO the caller and the interface is single-method:

  ```go
  // ForgeCaller executes one agent-initiated forge call as a resolved caller
  // account. sessionID rides along because the DL-050 Author stamp requires it.
  type ForgeCaller interface {
      ExecuteForgeCallAsAccount(ctx context.Context, caller store.AccountID,
          sessionID string, call *compassv1internal.ForgeCallRequest,
      ) (*compassv1internal.ForgeCallResult, error)
  }
  ```

  The hub keeps ONLY the security-critical resolution edge; everything
  domain-shaped lives behind the seam. (`sessionID` is a parameter, not a lookup
  key for authority: the account is already resolved; the session id feeds
  `Author.SessionID` per `owner.go:64` header grammar.)

- **`server/forge.go`** — `forgeService`, the `ForgeCaller` implementation
  (sibling of `newBoardService`, `board.go:110`, wired in
  `wireHubServiceCycles`, `sinks.go:89-92`). Per call it:
  1. resolves `caller` → `forge.Author{AgentHandle, OwnerHandle, SessionID}`
     from the store (agent handle + `owner_user_id`'s handle);
  2. selects the Provider AND the credential role from a registry keyed by the
     request's addressing (§4 below) — the REVIEWER client for the
     `submit_review` arm, the AUTHOR client for every other write (F1);
  3. on a WRITE arm: stamps via `forge.StampOwner(body, author, bodyLimit)` —
     the first production caller of the chokepoint (DL-050); `ErrBodyTooLarge`
     maps to an in-band `invalid_argument` error naming the overage
     (owner.go:100-102: header bytes "RESERVED against it FIRST");
  4. calls the Provider write; flattens `*forge.StatusError` 403/404 into one
     byte-identical in-band error (provider.go:218-220's stated purpose),
     `ErrUnsupported` into `code:"unimplemented"` naming the provider, and a
     rate-limit signal into `code:"resource_exhausted"` + `retry_after_ms`;
  5. on `create_issue`/`create_pull_request`: first consults the F3
     idempotency memo — a hit on `(agent_account_id, client_request_id)`
     returns the recorded coordinate with NO provider call; on a miss it calls
     the provider, then on success writes the DL-055 ownership-index row
     (coordinate + agent + owner + session) AND the memo in the SAME ordered
     step — write-after-forge-success, so "a stamp failure must not leave an
     orphan row, and a row must never be written for an artifact the forge
     rejected" (#995 design.md:2487-2488); a retry after a successful create is
     deduped, a retry after a failed create re-attempts;
  6. on READ arms (`get_issue`/`list_issues`/`get_pull_request`): answers from
     the issue projection/store for a TRACKED artifact, composing a live
     Provider fetch + `TranslateIssue` ONLY for an artifact the store does not
     track — the amendment's OQ-A ruling verbatim: "returning a fully-populated
     canonical `Issue`/`PullRequest` … for a tracked artifact, composing a live
     forge fetch + `TranslateIssue` only for an artifact the store does not
     track (subset shape, `id` empty ⇒ not addressable by `UpdateIssueState`)"
     (amendment design.md:479-482);
  7. on `subscribe`/`unsubscribe`: durable row ops on the existing
     `agent_forge_subscriptions` table (0001_init.sql:547; writer-less today
     per DL-163). Change DETECTION, notification delivery, AND seeding
     `forge_artifact_cursors` stay in the forge-poll-driver lane (DL-053) —
     this record only writes the subscription rows (A8 pin).

### 2. Runner leg: `Gateway.Forge`

`go/internal/runner/gateway/forge.go` mirrors `lifecycle.go:54-82` verbatim in
shape: resolve container → bound session (`g.sessions.Session(g.containerName)`),
fail closed `CodePermissionDenied` when unbound/empty ("never a forward with an
empty session id", lifecycle.go:16-18), forward
`RelayForgeCallRequest{SessionId, Call: req.Msg}`, surface a nil result as
`CodeInternal`. The Runner asserts NO account (DL-049 comment,
`agent_gateway.connect.go:113-116`).

### 3. GitHub Provider writes + the review surface

The four declared writes (`provider.go:202-207`) are implemented on the
existing hand-rolled `*GitHub` client (github.go — stdlib-only per its OQ-6
note, :51-53), sharing its `TokenSource`, `mapErrorResponse` (:361-366:
401/bad-creds-403 → `*StatusError` + token invalidate; rate-limit 403/429 →
`ErrBudgetExhausted` + gate), and rate-budget gate:

- `CreateIssue` → `POST /repos/{repo}/issues` (title, body, labels)
- `CommentOnIssue` → `POST /repos/{repo}/issues/{n}/comments`
- `CreatePullRequest` → `POST /repos/{repo}/pulls` (title, body, head, base, draft)
- `CommentOnPullRequest` → `POST /repos/{repo}/issues/{n}/comments` (GitHub
  models PR conversation comments as issue comments)

The NEW review surface (no arm, no Provider method, no GitHub mapping exists
today) adds (OQ-1/OQ-2 — resolved 2026-08-17, §Resolved decisions):

- proto: a `submit_review` oneof arm + `SubmitReviewRequest` (verdict, body,
  optional inline comments) and a `ReviewRef` write-ack in `forge.proto`
  mirroring `CommentRef` (forge.proto:48-54) — canonical-reference-only on the
  wire, per DL-069/DL-092;
- `Provider.SubmitReview(ctx, repo, number, in SubmitReview) (SubmittedReview, error)`
  - `FakeProvider` support;
- GitHub mapping: one `POST /repos/{owner}/{repo}/pulls/{n}/reviews` with
  `event` ∈ APPROVE/REQUEST_CHANGES/COMMENT, `body`, and a `comments[]` array
  taking `path` + `line` + `side` (verified against the GitHub REST reviews doc,
  2026-08-17: "comments (array of objects) … `path` (required), `position`,
  `body` (required), `line`, `side`, `start_line`, `start_side`" —
  <https://docs.github.com/en/rest/pulls/reviews>).

The `submit_review` arm executes under the reviewer credential — a second
`server_only` declared secret holding a DISTINCT GitHub identity — never the
author credential (F1 ruling, Matt 2026-08-17): GitHub rejects APPROVE and
REQUEST_CHANGES from the PR's own author with a 422 (only COMMENT is allowed
from the author; critic-verified 2026-08-17), and every Compass-authored PR
has the author-credential account as its author (serve.go:761-762 builds the
one author client), so a distinct reviewer identity is what makes the full
verdict vocabulary usable. The StampOwner header stays uniform: the stamp
carries the ACTING agent regardless of which credential posts.

**Motivating consumer:** the `skill://review` loop — the adversarial review
agent that runs on every PR — posts its findings and verdict to the GitHub PR
through this `submit_review` path as the reviewer identity, so the human sees
the full review conversation on the PR itself, not just in the agent
transcript. The agent-side wiring is named in T8.

Only the review SUMMARY body is stamped; inline comment bodies ride unstamped
inside the stamped review (OQ-7 deferral). Unstamped is NOT unpoliced: the
Service STRIPS any pre-existing owner-header block from every inline comment
body before it goes on the wire (A6, T4) — ingestion parses owner headers out
of comment bodies (forge.proto:81 "Set for COMMENT: the new comment,
header-stripped, author-attributed"), so a hand-written fake header in an
inline body would otherwise be a display-attribution forgery vector.

### 4. Provider addressing: same oneof, envelope `ForgeRef`, typed degradation

With a second provider the bare `repo` string stops being self-describing
("owner/name" vs Linear team key). The proto reserved exactly this follow-up
(`agent_gateway.proto:263-264`: "an optional ForgeRef is a named additive
follow-up"). The decided design (OQ-4, resolved 2026-08-17):

- an OPTIONAL `compass.v1.ForgeRef forge` field on the `ForgeCallRequest`
  envelope (additive; outside the oneof). Unset → the default provider
  (the configured GitHub forge) — existing callers and the GitHub-first tool
  prompt keep working. Partial-`ForgeRef` semantics are pinned (A3): an empty
  `host` means the SELECTED provider's default host; a coordinate naming a
  provider/host the registry has not configured is an in-band
  `ForgeCallError{code:"not_found"}` naming the coordinate — never a panic,
  never a silent fall-through to the default provider;
- NO capability-negotiation RPC in v1: a call to an op the selected provider
  cannot serve returns the in-band `ForgeCallError{code:"unimplemented"}`
  naming provider + op, built on the existing `forge.ErrUnsupported`
  (provider.go:214-216) — the error IS the negotiation, and the agent's tool
  prompt documents per-provider capabilities statically.

### 5. Linear provider (read + write, issues half)

`go/internal/forge/linear.go` — a stdlib GraphQL client (matching github.go's
no-dependency posture) over `https://api.linear.app/graphql`, authenticated by
its own `server_only` declared secret (`LINEAR_FORGE_TOKEN` — the third forge
write secret beside the two GitHub credentials, DL-052) through the same
`TokenSource` seam (github.go:26-29). `repo` = the Linear TEAM key (e.g.
"SEA") — the proto's standing doc says "the project key on Linear"
(agent_gateway.proto:260-262): the SAME Linear identifier (the issue-prefix
key); this record standardizes on Linear's own term "team key" and T1 updates
the proto comment wording to match (A9); the client resolves key → team id
once and caches it.

- Implements: `Name` ("linear"), `CreateIssue` (`issueCreate`),
  `CommentOnIssue` (`commentCreate`), `GetIssue`, `ListIssues` (issues query
  filtered by team, mapping Linear's per-team `number`). The read half is
  LIVE, not dead code: with no Linear ingestion, every Linear artifact is
  store-untracked, so the handler's OQ-A untracked fallback (§1 step 6)
  serves every Linear read through these methods.
- Returns `forge.ErrUnsupported` for: `CreatePullRequest`,
  `CommentOnPullRequest`, `GetPullRequest`, `Checks`, `SubmitReview` — Linear
  has no PR/review concept; the Service maps these to the typed in-band
  `unimplemented` error (OQ-3). The canonical `PullRequest` surface is never
  fabricated on a Linear coordinate.
- Attribution (OQ-5 + OQ-8, resolved 2026-08-17): writes go through
  `StampOwner` unchanged (one chokepoint, no per-provider branch in the
  Service) AND set Linear's `createAsUser`/`displayIconUrl` on the mutation to
  the ONE general shared Compass app identity — a CONSTANT general-user
  handle, NOT the per-agent handle ("the whole point of the stamp is that you
  don't need to bill 40 individual user accounts", Matt 2026-08-17). Native
  Linear display shows the shared "via Application" identity for every agent;
  the machine-parseable per-agent owner truth lives in the StampOwner header.
  Both values are Server-chosen, so DL-050's unforgeability holds on both
  channels — the native channel is deliberately coarse, the stamp is the
  fine-grained one. The token is an OAuth actor=app token (Linear OAuth actor
  docs, verified 2026-08-17: "set the `createAsUser` attribute … in
  issueCreate or commentCreate mutations",
  <https://linear.app/developers/oauth-actor-authorization>). Degradation to
  stamp-only on a non-actor token is a stated DESIGN INTENT, not an asserted
  API behavior (A4): a boot-time actor-capability probe governs whether the
  provider sets `createAsUser`, and a failed probe emits the named log line
  `linear: actor attribution unavailable; degrading to stamp-only` — this
  record does not assert what Linear does with the field under a plain API
  key.
- Rate limits: a 429 (or Linear's complexity rejection) maps to
  `resource_exhausted` + `retry_after_ms` from the response headers.

### 6. The DL-055 ownership index (first writer)

No authored-artifact table exists (0001_init.sql holds only the four
subscription/cursor tables, :534-587). A new migration adds
`forge_authored_artifacts` (coordinate `(forge_provider, forge_host, repo,
kind, number)` + `agent_account_id` + `owner_user_id` + `session_id` +
`created_at_unix_ms`), FK-aligned with `agent_forge_subscriptions`
(0001_init.sql:547-562), plus a `client_request_id` column with a UNIQUE index
on `(agent_account_id, client_request_id)` — the F3 idempotency memo.
`forgeService` writes row + memo in ONE ordered step after a successful
`create_issue`/`create_pull_request` — DL-055's "written with the stamp",
ordered forge-success-then-row so no row exists for a rejected write — and the
create path consults the memo BEFORE calling the provider, returning the
recorded coordinate on a hit.

**Accepted crash window (A5):** forge-success → crash before the row+memo
write → the artifact exists on the forge with NO ownership row, PERMANENTLY —
DL-050 forbids trusting a parsed header for a later backfill (the header is
untrusted display, never authority). This is an ACCEPTED loss for v1. The F3
memo shares the same window: a create that succeeded on the forge but crashed
before row+memo means a retry with the same `client_request_id` finds no
memo → re-attempts → a possible duplicate artifact. The memo narrows the
duplicate window to exactly this crash gap (a crash BEFORE the provider call
or AFTER the memo write dedupes cleanly); it does not eliminate it. The
outbox/two-phase alternative is weighed and rejected in §Alternatives.

### Wiring

`serve.go` gains a provider registry: the existing GitHub client construction
(serve.go:761-762) is shared with the write path as the AUTHOR client (same
`TokenSource`, same budget gate — one client per forge coordinate); a SECOND
GitHub client is constructed for the reviewer credential
(`GITHUB_FORGE_REVIEWER_TOKEN`, F1) — the registry entry carries both roles
and the service picks the reviewer client for `submit_review` only; Linear is
constructed when its secret is configured; and `wireHubServiceCycles`
(sinks.go:89-92) adds
`hub.SetForgeCaller(newForgeService(...))`. Production GitHub wiring asserts
`var _ forge.Provider = (*forge.GitHub)(nil)` only once BOTH lanes' methods
exist (RIG-1728 owns `GetIssue`/`GetPullRequest`/`Checks`); see the dependency
notes in §Plan.

## Alternatives considered

### Per-op `ForgeCaller` methods (the literal `BoardCaller` shape)

`BoardCaller` exposes one method per op (`relay_board.go:39-41`). Copying that
here means an 11-method interface the hub re-dispatches onto — the hub would
carry a switch naming every forge op, duplicating dispatch on both sides of the
seam. Rejected: the hub's job is the resolution edge; a single
`ExecuteForgeCallAsAccount` keeps the guard order identical to the siblings
while the domain dispatch lives once, in the service.

### A live forge proxy for the read arms

Answering `get_issue`/`list_issues`/`get_pull_request` by calling the Provider
directly for TRACKED artifacts. Rejected — already ruled: the amendment froze
OQ-A option 3 ("read ops … answered from the projection/store", amendment
design.md:474-476), and a live proxy for artifacts the store already tracks
would spend the DL-053 rate budget the poll driver rations. The live fetch for
an UNTRACKED artifact is NOT this rejected proxy — it is the ruled fallback
half of the same OQ-A decision (amendment design.md:479-482), restored in
Approach §1 step 6.

### Reusing canonical `compass.v1.Review` as the submit_review result arm

The canonical `Review` (compass.proto:884-889) is a read-surface type with no
`url`/`id` — a write ack needs the reference (mirroring `CommentRef`'s "a write
ack sets only url + comment_id", forge.proto:45-46). Widening the read type
with ack fields would leak write-ack semantics into every ingested review.
Rejected in favor of a `ReviewRef` in forge.proto (OQ-1).

### Capability-advertisement RPC / per-provider oneof families

A `GetForgeCapabilities` call (or a Linear-specific call family) so agents
never hit an unsupported op. Rejected for v1: it adds a wire surface + an agent
round-trip to avoid an error the agent handles anyway (in-band, non-fatal,
self-describing), and DL-049 froze ONE sibling family. The typed
`unimplemented` error + static tool-prompt documentation carries the same
information at zero new surface. Revisit only if a third provider makes the
capability matrix genuinely dynamic (OQ-4).

### go-github / a GraphQL client library

github.go is deliberately stdlib-only (:51-53, "OQ-6: no go-github dependency…
the high-level library lacks the conditional-request + budget hook"). The write
methods and the Linear client stay on the same posture — a JSON POST is not
worth a dependency, and the budget/token seams are already built.

### An outbox / two-phase write for the DL-055 row

Closing the A5 crash window (forge-success → crash → no ownership row, no
idempotency memo) with a durable intent row written BEFORE the provider call
(outbox pattern: intent → forge call → confirm), or a reconciliation sweep.
Rejected for v1: it inverts the ordering DL-055 froze for a reason ("a row
must never be written for an artifact the forge rejected", #995
design.md:2487-2488) — an intent row IS a row for an artifact the forge may
yet reject, so the pattern needs a second state column plus a janitor for
orphaned intents, trading a narrow accepted loss for permanent operational
machinery. The crash gap is one process crash inside a millisecond-scale
window between the provider's 201 and one local transaction; at dogfood
volume the expected loss is ~zero. Revisit only if a periodic
authored-artifact reconciliation sweep becomes needed for other reasons.

## Plan

Ordering: T1 (proto) unblocks everything wire-shaped; T2 (GitHub writes) can
start in parallel with T1; T3 (review mapping) depends on T2, NOT T1 — its
domain types are Go-only, the real edge is T2 → T3 for the `doJSON` helper
(A7: the previously-drawn T1 → T3 edge was false); T4 (service) consumes
T1+T2+T3 interfaces; T5 (relay legs) consumes the `ForgeCaller` interface,
whose DEFINITION lives in runnerhub — T5 may land FIRST with the interface
frozen there and T4 implementing it (A7); T6 (Linear) depends on T2's
`Provider` widening, independent of T3/T4/T5; T7 (index) is independent after
T4's call-order contract; T8 (wiring + E2E) last.
Dependencies: T1 → T4; T2 → {T3, T4, T6}; T3 → T4; T4 ↔ T5 (interface in T5's
package, implementation in T4 — either lands first); T4 → {T7, T8};
{T5, T6, T7} → T8.

**Cross-lane compile-break (A7):** T2 widens the `Provider` INTERFACE
(`BodyLimit()`; T3 adds `SubmitReview`) — every other implementor
(`FakeProvider`, and RIG-1728's GitHub read lane asserting
`var _ forge.Provider` once complete) breaks at compile until updated. T2/T3
must update every in-tree implementor in the same slice and coordinate with
the RIG-1728 lane before merging.

Each task is one PR-sized slice with its own red→green test cycle
(rule://red-green-testing) and reviewer gate.

### T1 — Proto: `submit_review` arm, `ReviewRef` ack, envelope `ForgeRef`

Add to `proto/compass/v1/agent_gateway.proto`:

```proto
// in ForgeCallRequest (:214-227): new oneof arm + envelope addressing
message ForgeCallRequest {
  string call_id = 1;
  oneof call {
    // …existing arms 2-10 unchanged…
    SubmitReviewRequest submit_review = 11;
  }
  // Which forge the call addresses. UNSET selects the default (configured
  // GitHub) forge — additive, existing callers unchanged (the follow-up
  // :263-264 named). An unknown/unconfigured coordinate is an in-band
  // not_found ForgeCallError; an EMPTY host means the selected provider's
  // default host (A3) — never a panic, never a silent provider fall-through.
  compass.v1.ForgeRef forge = 12;
  // Whole-chain idempotency key for the CREATE arms (create_issue /
  // create_pull_request) — the SpawnPeerRequest.client_request_id precedent
  // (agent_gateway.proto:172 "whole-chain idempotency key (handler-level
  // join + Provision dedup)"). A retried create with the same key returns
  // the ORIGINAL artifact, never a duplicate. Distinct from call_id, which
  // is correlation-only (:209-210). Ignored on non-create arms. Field 13 is
  // collision-free: call_id=1, oneof arms 2-11, forge=12.
  string client_request_id = 13;
}

message SubmitReviewRequest {
  string repo = 1;               // REQUIRED; empty is invalid_argument
  uint64 pull_number = 2;
  string verdict = 3;            // "approve" | "request_changes" | "comment"
  string body = 4;               // WITHOUT the owner header; the Server stamps it
  repeated ReviewCommentInput comments = 5;  // inline comments; may be empty
}
message ReviewCommentInput {
  string path = 1;               // REQUIRED
  uint32 line = 2;               // new-file line number (GitHub `line`)
  string side = 3;               // "LEFT" | "RIGHT"; "" = RIGHT
  string body = 4;               // NOT stamped (rides inside the stamped review)
}
```

And to `proto/compass/v1/forge.proto` (the leaf, beside CommentRef :48-54):

```proto
// A reference to a submitted PR review — the write ack for submit_review.
// Mirrors CommentRef: url + id only on an ack (DL-069: no forge shape on wire).
message ReviewRef {
  string url = 1;
  uint64 review_id = 2;
  string verdict = 3;            // echo of the applied verdict
}
```

`ForgeCallResult` (:235-247) gains `ReviewRef review = 10;`.

Interfaces:

- Produces: `compassv1internal.ForgeCallRequest_SubmitReview`,
  `SubmitReviewRequest`, `ReviewCommentInput`, `ReviewRef`,
  `ForgeCallResult_Review`, `ForgeCallRequest.Forge` +
  `ForgeCallRequest.ClientRequestId` accessors — regenerated
  into `go/internal/gen/compass/v1` (buf.gen.internal-go.yaml) and the agent TS
  lane (buf.gen.agent-ts.yaml); NEVER the public lane (forge.proto header :4-10).
- Consumes: `compass.v1.ForgeRef` (compass.proto:784-790).

Test cycle: `buf lint` + `buf breaking` green; a Go compile-time use of every
new accessor. No behavior yet.

### T2 — GitHub Provider write methods

`go/internal/forge/github.go` (same file, same client struct :66-84):

Interfaces (implements the existing declarations verbatim, provider.go:202-207):

```go
func (g *GitHub) CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error)
func (g *GitHub) CommentOnIssue(ctx context.Context, repo string, number uint64, body string) (Comment, error)
func (g *GitHub) CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error)
func (g *GitHub) CommentOnPullRequest(ctx context.Context, repo string, number uint64, body string) (Comment, error)
```

- Endpoints: `POST {apiBase}/repos/{repo}/issues`, `…/issues/{n}/comments`,
  `…/pulls`, `…/issues/{n}/comments` respectively (PR conversation comments are
  issue comments on GitHub).
- Shared plumbing: `g.token` TokenSource, `g.mapErrorResponse` (:361-366),
  `g.recordBudget` (:300-305), the `resetAt` fail-fast gate (:71-79) — writes
  respect the same reserve so a write burst cannot starve the poll driver.
- A shared private `doJSON(ctx, method, url, in, out any) error` helper carries
  auth/budget/error mapping once; the read path is NOT refactored onto it in
  this slice (no RIG-1728 rework).
- Body limit: GitHub caps issue/comment bodies at 65536 CHARACTERS;
  `StampOwner` reserves BYTES (owner.go:125-127 counts with `len`, a byte
  count). `BodyLimit()` is defined in BYTES: export `func (g *GitHub)
  BodyLimit() int { return 65536 }` — a UTF-8 string's character count never
  exceeds its byte count, so enforcing 65536 bytes is strictly conservative
  under GitHub's character cap (A9: the unit is pinned as bytes; no
  conversion anywhere). Add `BodyLimit() int` to the `Provider` interface
  (FakeProvider: configurable, default 0 = unlimited) — this is the
  cross-lane compile-break named in §Plan.

Test cycle: `httptest`-backed (github_test.go convention): per method — request
body/URL/auth golden, 201 happy path decoding into the domain type, 403-rate
vs 403-creds vs 404 mapping, budget-gate fail-fast.

### T3 — GitHub `SubmitReview` + the Provider review method

`go/internal/forge/provider.go` additions:

```go
// SubmitReview is the input to Provider.SubmitReview. Body is PRE-stamp
// (the Service stamps it); Comments ride unstamped inside the stamped review.
type SubmitReview struct {
    Verdict  string          // "approve" | "request_changes" | "comment"
    Body     string
    Comments []ReviewCommentInput
}
type ReviewCommentInput struct {
    Path string
    Line uint32
    Side string // "LEFT" | "RIGHT"; "" = RIGHT
    Body string
}
// SubmittedReview is the write ack a provider returns.
type SubmittedReview struct {
    ID      uint64
    URL     string
    Verdict string
}
// added to the Provider interface:
SubmitReview(ctx context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error)
```

GitHub mapping: `POST /repos/{repo}/pulls/{number}/reviews` with
`{"event": "APPROVE"|"REQUEST_CHANGES"|"COMMENT", "body": …, "comments":
[{"path", "line", "side", "body"}]}` (GitHub REST reviews doc, verified
2026-08-17). Verdict validation (unknown → error before any wire call) lives
here; the Service re-maps it in-band. `FakeProvider` gains the method +
scripted result/error (fake.go conventions :37-61).

Test cycle: request golden incl. event mapping and comments array; empty-
comments omits the array; unknown verdict errors with zero HTTP calls;
empty-body COMMENT and empty-body REQUEST_CHANGES rejected client-side with
zero HTTP calls (GitHub requires a body for both; APPROVE may be bodyless —
A2); an off-diff inline comment (path/line outside the diff) drives the
mocked 422 response through the mapping (→ `*StatusError{422}`, A2);
403/404/rate mapping.

Depends: T2 (the `doJSON` helper + the shared `Provider`-widening
coordination). NO T1 edge: the domain types here are Go-only, T1 parallel OK
(A7 — the earlier T1 → T3 dependency was false).

### T4 — `forge.Service`: the write chokepoint (`ForgeCaller` impl)

`go/server/forge.go` (package server, sibling of board.go):

Interfaces:

```go
// in runnerhub (relay_forge.go, T5 — the interface DEFINITION lands there;
// this task implements it):
type ForgeCaller interface {
    ExecuteForgeCallAsAccount(ctx context.Context, caller store.AccountID,
        sessionID string, call *compassv1internal.ForgeCallRequest,
    ) (*compassv1internal.ForgeCallResult, error)
}

// server/forge.go:
func newForgeService(st *store.Store, issueBrd *board.IssueProjection,
    providers *forgeProviderRegistry) *forgeService
// forgeProviderRegistry maps a ForgeRef coordinate (provider, host) to a
// forge.Provider + its BodyLimit, carrying TWO GitHub clients per
// coordinate — author and reviewer roles (F1): submit_review dispatches on
// the reviewer client, every other write on the author client. nil/unset
// ref resolves the default entry.
func (s *forgeService) ExecuteForgeCallAsAccount(…) // per-arm dispatch
```

Behavior (the numbered contract in §Approach 1): author resolve → provider +
credential-role select (reviewer client for `submit_review`, author client
otherwise — F1) → idempotency-memo lookup (create arms: a hit on
`(agent_account_id, client_request_id)` returns the recorded coordinate with
ZERO provider calls — F3) → stamp (writes) → provider call → error flatten →
DL-055 row + memo in one ordered step (creates) → result arm. Read arms
answer from `issueBrd`/store for tracked artifacts, live Provider fetch +
`TranslateIssue` for untracked (OQ-A, §Approach 1 step 6);
subscribe/unsubscribe write `agent_forge_subscriptions` rows — row writes
ONLY: change detection, notification delivery, and seeding
`forge_artifact_cursors` stay in the forge-poll-driver lane (DL-053, A8).
Inline review-comment bodies are STRIPPED of any owner-header block before
the provider call (never stamped — A6), closing the display-attribution
forgery vector: ingestion parses owner headers out of comment bodies
(forge.proto:81), so a hand-written header in an unstamped body would
otherwise impersonate another agent.
Authz posture (A8): inherited from the board path — "MVP scope ships no scope
rejection (single-trust-domain, Resolved decision 2)" (relay_board.go:37-38);
no per-op scope check in v1.
Error mapping table (single source): `ErrBodyTooLarge` → `invalid_argument`
(names the overage); `*StatusError{403}` ≡ `*StatusError{404}` → byte-identical
`not_found` (the #995 T2 flattening); `*StatusError{422}` →
`invalid_argument` carrying the forge's validation message (e.g. an off-diff
inline comment path/line — a genuinely invalid submission; NOT the
author-approving-own-PR case, which the F1 reviewer credential dissolves);
`ErrUnsupported` → `unimplemented` naming provider+op;
`ErrBudgetExhausted`/429 → `resource_exhausted` + `retry_after_ms`; empty
`repo` → `invalid_argument` before any store/provider touch; empty account →
error with ZERO provider calls; an unset/unknown oneof arm → the
`executeBoardCall` convention (relay_board.go:115-119: `default:` →
`connect.CodeInvalidArgument`, "board call has no set_issue_state variant
set") — a Connect error, not in-band, because an unset arm is a malformed
request, not a tool failure (A1).

Test cycle (against `FakeProvider`, mirroring #995's T2 list design.md:1753-1761):
every write stamped (header present, forged header in input replaced); inline
review-comment bodies carrying a hand-written owner header come out STRIPPED
(A6); read bodies never contain `compass:owner`; zero provider calls on empty
account; 403≡404 byte-identical; 422 → invalid_argument; unsupported →
unimplemented; body at exactly `limit − len(header)` stamps, one more errors
without a provider call; an unset oneof arm → CodeInvalidArgument (A1); a
create retried with the same `client_request_id` returns the original
coordinate with zero provider calls, and a retry after a FAILED create
re-attempts (F3); `submit_review` dispatches on the reviewer client,
`create_issue` on the author client (F1). Session ids in these tests MUST
satisfy the owner.go:40 handle grammar (`^[a-z0-9][a-z0-9-]{0,38}$`) — the
stamp interpolates the session id into the header, so a non-conforming id is
a StampOwner error; the format coupling is pinned here, not discovered in
review (A9).

Depends: T1 (result arm + `client_request_id` field), T2 (BodyLimit on
Provider), T3 (SubmitReview types), T7's store surface for the memo
lookup/row write (the store slice itself is independent and may land in
either order behind the `Store` methods).

### T5 — Relay legs: `Hub.RelayForgeCall` + `Gateway.Forge`

Interfaces:

```go
// go/internal/runnerhub/relay_forge.go
func (h *Hub) SetForgeCaller(c ForgeCaller)          // hub.go setter idiom :458-474
func (h *Hub) RelayForgeCall(ctx context.Context,
    req *compassv1internal.RelayForgeCallRequest,
) (*compassv1internal.RelayForgeCallResponse, error)
// go/internal/runnerhub/handler.go: route the RPC (sibling of :203)
func (h *Handler) RelayForgeCall(ctx context.Context,
    req *connect.Request[compassv1internal.RelayForgeCallRequest],
) (*connect.Response[compassv1internal.RelayForgeCallResponse], error)

// go/internal/runner/gateway/forge.go
func (g *Gateway) Forge(ctx context.Context,
    req *connect.Request[compassv1internal.ForgeCallRequest],
) (*connect.Response[compassv1internal.ForgeCallResult], error)
```

Guard order and error split copied from relay_board.go:50-57 /
lifecycle.go:54-82 (the deliberate structural mirror those files' `nolint:dupl`
comments name). The Gateway needs a `ForgeRelay` client field beside its
`CommsRelay`/lifecycle clients, threaded through the gateway constructor.

Test cycle: hub — nil-caller Unavailable (bound AND unbound session), unbound
session NotFound, resolved-account attribution via a fake ForgeCaller, in-band
error passthrough, call_id echo (relay_board_test.go conventions). Gateway —
unbound-session PermissionDenied, forward carries the bound session id
verbatim, nil result → CodeInternal (lifecycle_test.go conventions).

Depends: T4 (interface shape; a fake caller suffices for this task's tests, so
T5 can land before T4 if the interface is frozen in T5 and T4 implements it —
the interface DEFINITION lives in runnerhub either way).

### T6 — Linear provider (read + write)

`go/internal/forge/linear.go` + `linear_test.go`:

Interfaces:

```go
type LinearConfig struct {
    Token  TokenSource   // required (DL-052: its own server_only declared secret, LINEAR_FORGE_TOKEN)
    Client *http.Client  // nil -> default with sane timeout
    // Host is fixed "linear.app" (ForgeRef.host constant per compass.proto:788-789).
}
func NewLinear(cfg LinearConfig) *Linear
func (l *Linear) Name() string // "linear"
func (l *Linear) BodyLimit() int
// Implemented: CreateIssue, CommentOnIssue, GetIssue, ListIssues
// ErrUnsupported: CreatePullRequest, CommentOnPullRequest, GetPullRequest,
//                 Checks, SubmitReview
var _ Provider = (*Linear)(nil)
```

The read half (`GetIssue`/`ListIssues`) is LIVE-SERVED, not dead code: there
is no Linear ingestion, so every Linear artifact is UNTRACKED by the store,
and the handler's OQ-A untracked-artifact fallback (§Approach 1 step 6,
amendment design.md:479-482) routes every Linear read through this provider's
live fetch + `TranslateIssue` (subset shape, empty `id` ⇒ not addressable by
`UpdateIssueState`).

- GraphQL over `POST https://api.linear.app/graphql`, stdlib-only. `repo` = the
  team KEY; resolved to team id via a cached `teams(filter:{key:{eq:$key}})`
  lookup (mutex-guarded map; a miss is an in-band not_found).
- `CreateIssue` → `issueCreate(input:{teamId, title, description, labelIds?,
  createAsUser, displayIconUrl?})`; `CommentOnIssue` →
  `commentCreate(input:{issueId, body, createAsUser})`. `createAsUser` carries
  the ONE general shared Compass app identity — a constant, NOT the per-agent
  handle (OQ-8 ruling, Matt 2026-08-17); the per-agent owner truth rides the
  StampOwner header. When the boot-time actor-capability probe fails (a
  non-actor token), the provider omits the field and logs the named
  degradation line (A4, §Approach 5). Label names→ids resolution: v1 passes
  labels through only when resolvable, else returns invalid_argument naming
  the unknown label.
- Domain mapping: Linear issue → `forge.Issue{Number: issue.number (per-team),
  State: workflow-state-type → "open"/"closed" (completed/canceled → closed),
  ForgeAccount: creator displayName-or-app, URL: issue.url, UpdatedAt}`.
- Errors: HTTP 400 GraphQL errors → `*StatusError{Status:400, Message:
  first error message}`; 401 → StatusError + token Invalidate; 429 →
  budget-exhausted mapping with `Retry-After`.

Test cycle: `httptest` GraphQL stub — request-body goldens (query + variables,
incl. the constant general-user `createAsUser`), team-key cache (second call =
one lookup), state mapping
table, ErrUnsupported for all five PR-family methods (zero HTTP calls),
401/429 mapping.

Depends: T2 (Provider.BodyLimit addition). Independent of T3/T4/T5.

### T7 — DL-055 ownership index: migration + store methods

Interfaces:

```go
// go/internal/store (new migration 0002_forge_authored_artifacts.sql)
type AuthoredArtifact struct {
    Provider ForgeProvider; Host, Repo string
    Kind     ForgeArtifactKind // reuse the store-side kind (issue|pr)
    Number   uint64
    AgentAccountID AccountID; OwnerUserID AccountID; SessionID string
    ClientRequestID string // F3 idempotency memo key; "" = no key supplied
    CreatedAtUnixMS int64
}
func (s *Store) RecordAuthoredArtifact(ctx context.Context, a AuthoredArtifact) error // idempotent upsert on coordinate; row + memo in ONE statement
func (s *Store) AuthoredArtifactByRequestID(ctx context.Context, agent AccountID, clientRequestID string) (AuthoredArtifact, bool, error) // the F3 dedup lookup
func (s *Store) ListAuthoredArtifactsByAgent(ctx context.Context, agent AccountID) ([]AuthoredArtifact, error)
```

Schema: PK `(forge_provider, forge_host, repo, kind, number)`; a
`client_request_id` column (nullable — NULL when the caller supplied no key)
with a UNIQUE partial index on `(agent_account_id, client_request_id) WHERE
client_request_id IS NOT NULL` — the F3 memo; FKs
`agent_account_id → agent_accounts`, `owner_user_id → user_accounts`, both
`ON DELETE RESTRICT` (0001_init.sql FK discipline, :446-447). CHECK on
forge_provider IN (1,2,3,4) matching :535.

Test cycle: DL-174 pair — in-memory reference + pgtest suite (insert,
idempotent re-insert, FK restrict, by-agent scan, memo lookup hit/miss,
UNIQUE violation on a duplicate `(agent, client_request_id)`). T4
integration: row+memo written on create success, NO row on provider error
(the #995 :2487-2488 negative leg), retry-after-success deduped.

Depends: T4 (call-order contract only; the store slice itself is independent).

### T8 — Wiring + E2E

- `serve.go`: build the provider registry — reuse the poll driver's GitHub
  client when configured (serve.go:761-762; else construct one for the write
  path with the same TokenSource idiom) as the AUTHOR client; construct a
  SECOND GitHub client for the reviewer credential
  (`GITHUB_FORGE_REVIEWER_TOKEN`, a second `server_only` declared secret via
  the `validateForgeSecret` pattern :779-790 — F1); construct Linear iff its
  secret is declared (`LINEAR_FORGE_TOKEN`); add
  `hub.SetForgeCaller(newForgeService(st, issueBrd, registry))` to
  `wireHubServiceCycles` (sinks.go:89-92).
- `go/internal/runner`: thread the `ForgeRelay` client into the Gateway
  constructor (mirroring the lifecycle client).
- E2E (the #995 T9 shape, design.md:2493-2496): over the real socket with
  `FakeProvider` — create → stamped → DL-055 row+memo; a create retried with
  the same `client_request_id` → original coordinate, one provider call total
  (F3); write-forgery (hand-written header for another agent comes out
  stamped for the caller); a forged owner header inside an inline
  review-comment body comes out STRIPPED (A6); 403≡404; no-live-session fails
  closed; submit_review round-trip dispatching the reviewer client — a
  `verdict:"approve"` on a PR created by the author credential succeeds
  (reviewer ≠ author, the F1 acceptance proof); a Linear-addressed
  `create_pull_request` returns in-band `unimplemented`.
- Agent tool prompt: the compass-agent forge tool description enumerates ops +
  per-provider support (a TS-side text change; no wire change).
- `skill://review` loop wiring (the F1 motivating consumer): the review
  agent's PR-review step posts its findings/verdict through `submit_review`
  (reviewer identity) instead of transcript-only output. The skill/prompt
  change is agent-side, OUTSIDE this server record's code scope — named here
  as an explicit consumer follow-up so it is not lost; the server-side
  surface it consumes is fully delivered by this record.

Depends: T4, T5, T6, T7.

## Tasks

- [ ] **T1** *(proto)* — `submit_review` arm + `SubmitReviewRequest`/
  `ReviewCommentInput`, `ReviewRef` in forge.proto, `ForgeCallResult.review`
  arm, envelope `ForgeRef forge = 12` + `client_request_id = 13` (F3);
  proto-comment "project key" → "team key" wording fix (A9); regen
  internal-go + agent-ts; buf lint/breaking green.
- [ ] **T2** *(compass-server)* — GitHub Provider writes (`CreateIssue`/
  `CommentOnIssue`/`CreatePullRequest`/`CommentOnPullRequest`) + `BodyLimit()`
  (BYTES) on the Provider interface (cross-lane compile-break coordinated,
  A7); httptest goldens + error-mapping suite.
- [ ] **T3** *(compass-server)* — `Provider.SubmitReview` + domain types +
  GitHub reviews-API mapping + FakeProvider support; empty-body-verdict +
  off-diff-comment tests (A2).
- [ ] **T4** *(compass-server)* — `forgeService` (`ForgeCaller` impl): stamp
  chokepoint, reviewer/author credential dispatch (F1), create-idempotency
  memo lookup (F3), inline-comment owner-header STRIP (A6), error flatten
  (incl. 422 + unset-arm rows, A1/A2), OQ-A read arms incl. the untracked
  live fetch, subscription rows, DL-055 row+memo call; full FakeProvider
  suite.
- [ ] **T5** *(compass-server)* — `Hub.RelayForgeCall` + `SetForgeCaller` +
  handler route + `Gateway.Forge`; guard-order + attribution tests.
- [ ] **T6** *(compass-server)* — Linear provider (issues half): writes +
  LIVE reads via the OQ-A untracked fallback, ErrUnsupported PR family,
  general-user `createAsUser` + boot-time actor probe (OQ-8/A4); httptest
  GraphQL suite.
- [ ] **T7** *(compass-server)* — `forge_authored_artifacts` migration
  (+ `client_request_id` memo column/index, F3) + store methods + pgtest
  pair.
- [ ] **T8** *(compass-server + compass-agent)* — serve.go registry wiring
  (author + reviewer GitHub clients, Linear), Runner client threading, socket
  E2E (incl. the reviewer-verdict proof), tool-prompt capability text,
  `skill://review` consumer follow-up named.

## Resolved decisions (2026-08-17)

The design-critic ratified OQ-1..4 as surviving attack; Matt ruled F1, F3,
and OQ-8 (which corrects OQ-5) the same day. Recorded here as the record's
decisions; the only live questions below are two non-load-bearing deferrals.

- **OQ-1 — Review proto + Provider shape: (a).** `submit_review` arm +
  `SubmitReviewRequest` + a `ReviewRef` write ack in forge.proto mirroring
  `CommentRef`; Provider gains `SubmitReview`. Why: no forge shape on the
  wire (DL-069/DL-092), and the read-side `Review` stays unpolluted by ack
  fields.
- **OQ-2 — Inline comments in v1: (a).** Verdict + summary body + optional
  inline `comments[]` in ONE call, never a PENDING review. Why: GitHub's
  create-review endpoint takes exactly this in one POST, and the review-loop
  consumer needs inline comments to be useful; thread replies stay deferred.
- **OQ-3 — Linear PR/review degradation: (a).** Typed in-band `unimplemented`
  error built on `forge.ErrUnsupported`. Why: provider capability is a
  provider fact; the canonical PullRequest surface is never fabricated on a
  Linear coordinate.
- **OQ-4 — Addressing: (a).** Same oneof + optional envelope `ForgeRef` +
  typed unsupported-op error; no negotiation RPC. Why: DL-049 froze one
  family; the error is self-describing; two providers need no dynamic
  discovery surface.
- **OQ-5 — Linear stamp semantics: (a), corrected by OQ-8.** Stamp via
  `StampOwner` unchanged AND set `createAsUser` — to the ONE general shared
  app identity, NOT the agent handle. Why: the chokepoint stays
  provider-uniform; the native channel is deliberately coarse and the stamp
  is the fine-grained per-agent truth.
- **OQ-8 — Linear token kind: OAuth actor=app (Matt).** `LINEAR_FORGE_TOKEN`
  holds an OAuth actor=app token; `createAsUser` = the general shared user.
  Why: "the whole point of the stamp is that you don't need to bill 40
  individual user accounts."
- **F1 — Second reviewer credential + review-loop consumer (Matt).** A second
  `server_only` declared secret (`GITHUB_FORGE_REVIEWER_TOKEN`) holds a
  DISTINCT GitHub reviewer identity; `submit_review` executes as the
  reviewer, every other write as the author; the `skill://review` loop posts
  its per-PR review through this path as the reviewer. Why: GitHub 422s
  APPROVE/REQUEST_CHANGES from a PR's own author, so a distinct identity is
  what makes the full verdict vocabulary usable — and it puts the review
  conversation on the PR where the human reads it.
- **F3 — Create idempotency (Matt).** `ForgeCallRequest.client_request_id`
  (field 13) is a whole-chain idempotency key for the create arms, mirroring
  `SpawnPeerRequest.client_request_id` (agent_gateway.proto:172 "whole-chain
  idempotency key (handler-level join + Provision dedup)"); the dedup memo is
  a `client_request_id` column + UNIQUE `(agent_account_id,
  client_request_id)` index on `forge_authored_artifacts`, written in the
  same ordered step as the DL-055 row. Why: a retried create must return the
  original artifact, and the spawn precedent already froze the shape.

## Open Questions

Two explicit NON-LOAD-BEARING deferrals survive the freeze; everything else
is recorded in §Resolved decisions. Neither blocks any task, interface, or
wire shape.

### OQ-6 (non-load-bearing, deferred) — Shared rate budget between poll driver and write path

T8 reuses the poll driver's GitHub client as the author client, so writes and
polls share one budget gate (serve.go:761-762 builds exactly one). A heavy
write burst could starve the poll cycle at the shared reserve floor
(`const reserve = 10`, github.go:89). Non-load-bearing because the failure
mode is graceful and self-healing — the gate self-clears ("Once now() passes
resetAt the gate re-opens, so a wedged window self-clears after the reset",
github.go:77) and the budget math is per-token regardless of client split, so
splitting clients later changes no interface or schema. Revisit if dogfood
shows poll starvation.

### OQ-7 (non-load-bearing, documented default) — Stamp scope on a review

Only the review SUMMARY body is stamped; inline `comments[].body` ride
unstamped inside the stamped review. Rationale: one review = one artifact =
one attribution; stamping every inline comment costs `len(header)` per
comment against no additional attribution truth, and the review's forge
author is the same reviewer account for all of them. The deferral is SOUND
because A6 closes the forgery vector: T4 STRIPS any owner-header block from
inline comment bodies before the wire, so an unstamped body can never carry a
forged attribution header into ingestion (which parses headers out of comment
bodies, forge.proto:81). If Matt later wants per-comment stamps, it is a
one-line loop in T4 — flagged, not blocking.

## Ledger delta

Drafted for the driver to apply to `docs/designs/product/DECISIONS.md` in the
design PR (this record does not edit the ledger).

**New rows** (DL-200..206; append to `## Comms & tools`, after DL-182):

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-200 | The forge write path is served by a single-method `ForgeCaller` seam (`ExecuteForgeCallAsAccount(ctx, caller, sessionID, call)`) behind `Hub.RelayForgeCall` with the RelayBoardCall guard order (nil-caller CodeUnavailable before resolution; unbound session CodeNotFound; tool errors in-band); the hub keeps only the resolution edge, the oneof dispatch + stamping + provider selection live in the server-side `forgeService` — the first production `forge.StampOwner` caller (DL-050) | Active (Matt, 2026-08-17) | [forge write path §1](compass-forge-write-path/design.md#1-the-forgecaller-seam--relayforgecall-server-leg-the-dl-050-chokepoint) |
| DL-201 | The review surface is a `submit_review` arm on the existing `ForgeCallRequest` oneof (verdict + summary body + optional inline path/line/side comments, one POST, never a PENDING review) acked by a `ReviewRef` reference in forge.proto mirroring `CommentRef`; the canonical read-side `compass.v1.Review` is untouched (DL-069/DL-092 hold: no forge-shaped wire type, write acks are references). The arm executes under a DISTINCT reviewer GitHub identity — a second `server_only` declared secret (F1) — so APPROVE/REQUEST_CHANGES/COMMENT are all usable on Compass-authored PRs (GitHub 422s an author's self-review verdicts); the motivating consumer is the `skill://review` loop posting its per-PR review to the PR as that reviewer. Amends DL-052 (the reviewer credential joins the author credential as a second `server_only` secret; the Server-holds-write-creds core stands) | Active (Matt, 2026-08-17) | [forge write path §3](compass-forge-write-path/design.md#3-github-provider-writes--the-review-surface) |
| DL-202 | Multi-provider addressing is an optional `ForgeRef forge` field on the `ForgeCallRequest` envelope (unset = the configured default GitHub forge — the additive follow-up agent_gateway.proto reserved); there is no capability-negotiation RPC: an op the addressed provider cannot serve returns the in-band `ForgeCallError{code:"unimplemented"}` built on `forge.ErrUnsupported`, and the agent tool prompt documents the static per-provider capability matrix | Active (Matt, 2026-08-17) | [forge write path §4](compass-forge-write-path/design.md#4-provider-addressing-same-oneof-envelope-forgeref-typed-degradation) |
| DL-203 | The Linear provider (`go/internal/forge/linear.go`, stdlib GraphQL, team key as `repo`) serves the issues half read+write — the read half served live via the OQ-A untracked-artifact fallback (no Linear ingestion exists) — and returns `ErrUnsupported` for the whole PR/review family — the canonical PullRequest surface is never fabricated on a Linear coordinate; its write credential is its own `server_only` declared secret (DL-052). Amends DL-051 (Linear moves from a deferred issues-only follow-on to in-scope issues read+write; the swappable-`Provider` framing and the PR/review-family-unsupported core stand) | Active (Matt, 2026-08-17) | [forge write path §5](compass-forge-write-path/design.md#5-linear-provider-read--write-issues-half) |
| DL-204 | Linear attribution is dual-channel with deliberate granularity: every write passes through `StampOwner` unchanged (the one chokepoint; the header carries the fine-grained PER-AGENT owner truth) AND sets Linear's native `createAsUser` to the ONE general shared Compass app identity (coarse native display, never per-agent; the token is OAuth actor=app, degrading to stamp-only via a named boot-time capability probe on a non-actor token); both values are Server-chosen so DL-050 unforgeability holds on both channels | Active (Matt, 2026-08-17) | [forge write path §5](compass-forge-write-path/design.md#5-linear-provider-read--write-issues-half) |
| DL-205 | The DL-055 ownership index materializes as `forge_authored_artifacts` (PK = forge coordinate; agent/owner/session columns; ON DELETE RESTRICT FKs), written by `forgeService` strictly after forge write success — no row for a rejected write, no orphan row on a stamp failure | Active (Matt, 2026-08-17) | [forge write path §6](compass-forge-write-path/design.md#6-the-dl-055-ownership-index-first-writer) |
| DL-206 | Forge creates are idempotent under a caller-minted `ForgeCallRequest.client_request_id` whole-chain key (the SpawnPeerRequest.client_request_id precedent, agent_gateway.proto:172), deduped by a memo — a `client_request_id` column + UNIQUE `(agent_account_id, client_request_id)` index on `forge_authored_artifacts`, written in the SAME ordered step as the DL-055 row — so a retried create returns the original artifact, a retry after a failed create re-attempts, and the accepted residual window is exactly the forge-success→pre-commit crash gap (§Alternatives: outbox rejected) | Active (Matt, 2026-08-17) | [forge write path §6](compass-forge-write-path/design.md#6-the-dl-055-ownership-index-first-writer) |

**Amendments (append-only mechanic, DL-113→DL-096 precedent).** DL-051 and
DL-052 stay `Active` and are NOT reworded — the ledger row-status grammar
admits only `Active (<who>, YYYY-MM-DD)` or `Superseded by DL-<n> (…)`
(`design-ledger-gate` `index.ts:93-95`), and an amendment whose core stands is
carried by the NEW row that names the amended one, never an in-place edit:

- **DL-051** (forge adapter is `go/internal/forge` behind a swappable
  `Provider`, GitHub first, Linear issues-only) is amended by **DL-203**:
  Linear moves from a deferred issues-only follow-on to in-scope issues
  read+write; the swappable-`Provider` framing and the PR/review-family-
  unsupported core stand.
- **DL-052** (the Server holds forge write creds as a `server_only` declared
  secret) is amended by **DL-201**: a DISTINCT reviewer credential joins the
  author credential as a second `server_only` secret; the
  Server-holds-write-creds core stands.

Citation sweep obligation on merge: grep `per DL-051` / `DL-051` across the
tree (known sites include `compass.proto:781-782`'s enum comment "DL-051's
issues-only forge source") and update or file the follow-up.
