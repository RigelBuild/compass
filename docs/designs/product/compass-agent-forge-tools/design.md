# Compass agent forge tools

Status: Active

Design for the TypeScript agent-side leg of the forge surface: a `forge()`
method on the frozen `RunnerTransport` seam plus a native tool set (`forge.ts`)
that exposes all ten `ForgeCallRequest` arms to the containerized first-party
agent, one tool per arm (the two subscription arms ship now but return the
server's in-band `unimplemented` until the `agent_forge_subscriptions` store
writer lands), with a
multi-forge selector so an agent targets Linear as well as the default GitHub
forge, wired into the container entrypoint's `customTools`. This is
the direct forge sibling of the comms-tools record
([`compass-agent-comms-tools/design.md`](../compass-agent-comms-tools/design.md),
its T3) and the lifecycle tools (`packages/compass-agent/src/lifecycle.ts`):
same broker shape, same identity model, same registration path. Everything
below the agent — the Server write chokepoint, the hub relay, the Runner
gateway leg, and the proto carrier — is COMPLETE and frozen elsewhere
([`compass-forge-write-path/design.md`](../compass-forge-write-path/design.md));
this record consumes it and designs nothing on the Go side.

**Grounding.** This record and the code it describes live together in this
repository; citations resolve against this tree directly (`docs/…`, `go/…`,
`proto/…`, `packages/…`). Anchors were verified against this workspace at
authoring (2026-08-24). Named rather than blanket, per the comms-tools
convention: a line range is a claim about a commit and drifts as the file
grows, so load-bearing citations below name the symbol as well as the path.

Tracker: RIG-2672.

## Problem / Intent

The forge write path is built end-to-end and has no caller. The Server-side
chokepoint dispatches all eight domain arms
(`go/server/forge.go` `ExecuteForgeCallAsAccount` — createIssue,
commentOnIssue, createPullRequest, commentOnPullRequest, submitReview,
getIssue, listIssues, getPullRequest; only subscribe/unsubscribe return
`CodeUnimplemented`, gated on the poll-driver lane's subscription-store
writer), the hub relay exists (`go/internal/runnerhub/relay_forge.go`
`Hub.RelayForgeCall` over the `ForgeCaller` seam, mounted at
`go/server/serve.go` `hub.SetForgeCaller(forgeSvc)` via
`buildForgeWriteService`), the Runner gateway leg exists and is tested
(`go/internal/runner/gateway/forge.go` `Gateway.Forge`, fail-closed), and the
proto carrier + generated TS types exist
(`ForgeCallRequest`/`ForgeCallResult`/`ForgeCallError`,
`packages/compass-agent/src/gen/compass/v1/agent_gateway_pb.ts`).

But the agent cannot reach any of it: `RunnerTransport`
(`packages/compass-agent/src/transport/index.ts`) exposes `comms()` and
`lifecycle()` and no `forge()`, and no `forge.ts` exists (comms ships five
tools, lifecycle two, forge zero). This record designs the missing TS leg —
the transport method, the `ForgeBroker`, the ten native tools (the two
subscription tools built now over the stub arms) with their multi-forge
selector and per-tool prompt guidance, and the `cli.ts` wiring — and nothing
else.

## Approach

### Decisions (pre-resolved by Matt — not reopened below)

1. **Full surface: all ten `ForgeCallRequest` arms become tools.** Every arm
   the wire carries ships as an agent tool in this record — the eight arms
   `ExecuteForgeCallAsAccount` dispatches for real PLUS `subscribe`/
   `unsubscribe`. No partial MVP subset, no hidden arms.
2. **One tool per arm.** Single-purpose, well-documented tools — agents perform
   better with them — mirroring the comms/lifecycle one-tool-per-verb
   precedent. Names: `forge_get_issue`, `forge_get_pull_request`,
   `forge_comment_on_issue`, `forge_comment_on_pull_request`,
   `forge_submit_review`, `forge_create_issue`, `forge_create_pull_request`,
   `forge_list_issues`, `forge_subscribe`, `forge_unsubscribe`.
3. **`forge_subscribe`/`forge_unsubscribe` ship NOW, returning the server's
   in-band `unimplemented` until the writer lands.** The backend arms are
   `CodeUnimplemented` stubs (`go/server/forge.go`) pending the
   `agent_forge_subscriptions` store writer, which the poll-driver lane owns
   ([`compass-forge-poll-driver/design.md`](../compass-forge-poll-driver/design.md),
   DL-163: the tables land writer-less; that lane brings the writer). Matt
   ruled build-all: the tools ship now so the surface is stable and complete;
   the two arms simply error at runtime (rendered as a clean in-band
   `unimplemented`, with the per-tool guidance saying "not yet wired") until
   the writer lands, at which point they light up with no contract rework and
   no re-registration.
4. **V1 is multi-forge: the `ForgeRef` selector is exposed on every tool.**
   The wave needs GitHub AND Linear (it files Linear issues and opens GitHub
   PRs), and the substrate resolves a per-provider coordinate end to end
   (`forgeProviderRegistry.resolve`, `go/server/forge.go`; the co-equal Linear
   write provider `go/internal/forge/linear.go`, built + unit-tested). Every
   tool takes an optional `forge_provider` (+ optional `forge_host`); unset =
   the configured default GitHub forge (DL-202). Linear is issues-only
   (DL-051): `repo` = team key, PR/review arms return `unimplemented`. Matt
   ruled this in, overruling the earlier GitHub-only-v1 / ForgeRef-deferred
   scoping.

### The built substrate this composes with

- **The Server is the sole write chokepoint, and it is live.**
  `go/server/forge.go` `ExecuteForgeCallAsAccount` is the `ForgeCaller` entry
  point and per-arm dispatch: it fails a missing caller closed, dispatches the
  `call` oneof to the eight arm methods, and returns every domain outcome
  in-band on the `ForgeCallResult` (an unset arm is `CodeInvalidArgument`).
  Writes resolve identity (`resolveIdentity` — DL-050 stamp Author from the
  agent's own handle + owner handle + session id), stamp the owner header
  server-side, dedup creates on `client_request_id` (`dedup`, DL-206), and
  record the DL-055 ownership row (`record`). Reads are answered from the
  projection/store for tracked artifacts with a live-fetch fallback
  (`getIssue`/`getPullRequest`/`listIssues`, per the write-path record's OQ-A
  ruling). Nothing agent-side re-implements any of this.
- **The relay + gateway legs mirror comms/lifecycle exactly.**
  `go/internal/runnerhub/relay_forge.go` `Hub.RelayForgeCall` resolves the
  relayed `session_id` to its bound agent account (fail-closed `CodeNotFound`;
  no `ForgeCaller` wired → `CodeUnavailable`) and delegates under the RESOLVED
  account — never request-asserted, never admin.
  `go/internal/runner/gateway/forge.go` `Gateway.Forge` resolves
  container→session (fail-closed `CodePermissionDenied` when unbound) and
  forwards `RelayForgeCall{session_id, call}`; a nil result is `CodeInternal`
  ("Mirrors Lifecycle exactly", forge.go). The agent presents no token and
  asserts no account, so every tool below inherits the DL-050 attribution and
  the F1 author/reviewer credential roles with zero new authz code.
- **The proto carrier and its generated TS types exist.**
  `proto/compass/v1/agent_gateway.proto` `rpc Forge(ForgeCallRequest) returns
  (ForgeCallResult)` on `AgentGateway`; `ForgeCallRequest` carries `call_id`,
  the ten-arm `call` oneof (arms 2–11), an optional `ForgeRef forge` (unset =
  configured default GitHub forge, DL-202), and `client_request_id` (create
  arms only, DL-206). `ForgeCallResult` retypes the domain arms to canonical
  `compass.v1.Issue`/`PullRequest` (DL-069/DL-092) plus `CommentRef`/`ReviewRef`
  write acks (forge.proto) and the in-band `ForgeCallError{code, message,
  retry_after_ms}`. The TS gen is already in the internal agent tree
  (`packages/compass-agent/src/gen/compass/v1/agent_gateway_pb.ts`
  `ForgeCallRequest`/`ForgeCallResult` + the arm request schemas;
  `gen/compass/v1/forge_pb.ts` `CommentRef`/`ReviewRef`;
  `gen/compass/v1/compass_pb.ts` `Issue`/`PullRequest`). This record adds NO
  proto and regenerates nothing.
- **The transport seam is frozen and the client exists.** `RunnerTransport`
  (`packages/compass-agent/src/transport/index.ts`) is the agent's handle on
  the `AgentGateway` RPCs over the per-container Unix socket
  ([`compass-agent-runner-transport/design.md`](../compass-agent-runner-transport/design.md)
  Decisions #3–#4): `comms`/`lifecycle` are one-line unary delegations
  (`createUnixSocketTransport`: `comms: (req) => client.comms(req)`). The
  generated Connect client already carries the `forge` method (the service
  schema includes the `Forge` RPC), so adding `forge()` to the interface and
  the impl is one interface member + one delegation line — no new socket, no
  new Connect surface, no biome-carve change (the carve already scopes
  `packages/compass-agent/src/transport/**`).
- **The tool/broker shape is established twice over.** `comms.ts` and
  `lifecycle.ts` each define: a one-method structural transport subset
  (`CommsTransport`/`LifecycleTransport`) so tools depend on one method and a
  test fakes one method; a thin broker (`CommsBroker`/`LifecycleBroker`)
  delegating `call()` to the transport with a broker-scoped
  `#idempotencyNonce` and `idempotencyKey(toolCallId)` =
  `` `${nonce}:${toolCallId}` `` (the cross-session tool-call-id collision the
  comms record's errata documents); exported arktype `parameters` objects
  (non-blank `.narrow` bounds repeated in descriptions because `.narrow` has
  no JSON Schema form); a `*Failure(result, toolName, expected)` error mapper
  (in-band `error` arm → thrown `Error` with `attr(code)` + `flat(detail)`;
  wrong arm → protocol violation); and render-guarded output via the shared
  `attr`/`flat` (`render-guard.ts`). `forge.ts` mirrors all of it.
- **Registration is `customTools` at the composition root.** `cli.ts main()`
  constructs the brokers over the one transport
  (`new CommsBroker(transport)`, `new LifecycleBroker(transport)`), merges
  `nativeTools = [...createCommsTools(…), ...createLifecycleTools(…)]`, and
  passes `customTools: [...mcp.tools, ...nativeTools]` into
  `createAgentSession` — the natives flow through the
  customTools→state.tools→`#withNatives` path. Forge adds one broker and one
  spread to that list.

### Tool set and shape — ten native tools, one per arm

All ten tools live in one new `packages/compass-agent/src/forge.ts`,
authored as OMP `AgentTool`s with arktype parameters, closing over a
`ForgeBroker`. Approval levels follow the comms precedent (`comms.ts`:
mutations `approval: "write"`, reads `approval: "read"`; the container runs
headless with write-approval tools auto-executing, per the comms record):

| Tool | Arm (`call.case`) | Approval | Result arm |
| --- | --- | --- | --- |
| `forge_get_issue` | `getIssue` | read | `issue` |
| `forge_get_pull_request` | `getPullRequest` | read | `pullRequest` |
| `forge_list_issues` | `listIssues` | read | `issues` |
| `forge_comment_on_issue` | `commentOnIssue` | write | `issueComment` (CommentRef) |
| `forge_comment_on_pull_request` | `commentOnPullRequest` | write | `prComment` (CommentRef) |
| `forge_submit_review` | `submitReview` | write | `review` (ReviewRef) |
| `forge_create_issue` | `createIssue` | write | `issue` |
| `forge_create_pull_request` | `createPullRequest` | write | `pullRequest` |
| `forge_subscribe` | `subscribe` | write | `subscribed` (`unimplemented` until the writer lands) |
| `forge_unsubscribe` | `unsubscribe` | write | `unsubscribed` (`unimplemented` until the writer lands) |

Parameter surfaces map 1:1 onto the proto request messages
(`agent_gateway.proto` `CreateIssueRequest`…`SubmitReviewRequest`), with the
wire's documented bounds carried into descriptions:

- `repo` is REQUIRED on every tool (`"<owner>/<name>"` on GitHub, team key on
  Linear) — the wire rejects an empty `repo` as `invalid_argument` and the
  tool schema enforces non-blank up front, so the model learns the rule from
  the schema description rather than a server round-trip.
- Write bodies (`body`) are sent WITHOUT the owner header — the Server stamps
  it (DL-050). The per-tool prompt guidance says so explicitly ("never write
  an attribution header yourself; the server adds it"), because a model that
  has seen stamped bodies on the forge will otherwise imitate them and the
  stamp chokepoint would strip-then-restamp (correct but noisy).
- `forge_create_issue` and `forge_create_pull_request` set
  `client_request_id = broker.idempotencyKey(toolCallId)` (DL-206
  whole-chain dedup; broker-scoped nonce per the comms/lifecycle errata).
  The non-create arms send none — the field is ignored there by contract.
- `forge_submit_review.verdict` is a closed enum
  (`"approve" | "request_changes" | "comment"`) in the tool schema, so an
  invalid verdict never leaves the container; `comments[]` items carry
  `path` (required), `line`, optional `side` (`"LEFT" | "RIGHT"`, default
  RIGHT) per `ReviewCommentInput`.
- `forge_list_issues` bounds `limit` to `1..100` (`0`/omitted = server
  default 30, capped at 100 per the wire comment) and `state` to
  `"open" | "closed" | "all"` with omitted = open.
- **Create bodies are OPTIONAL; comment/review bodies are required.**
  `forge_create_issue` and `forge_create_pull_request` take `body?` — the
  Server stamps an owner header into the body (DL-050), so an empty agent
  body is well-formed and non-empty on the forge; forcing non-blank would
  make the model invent filler for a title-only issue. `comment`/`review`
  bodies carry no server-supplied content and stay required-non-blank.
- **`forge_submit_review` needs a body unless it approves.** GitHub rejects a
  `request_changes`/`comment` review with an empty body (the forge returns
  `invalid_argument`); the schema `.narrow`s `body` required unless
  `verdict = "approve"`, closing the rule in-container rather than on a
  server round-trip — the same schema-first rationale as the non-blank `repo`.
- **Multi-forge selector on every tool (`forge_provider` + `forge_host`,
  both optional).** The wire carries an optional `ForgeCallRequest.forge`
  (`ForgeRef`: `provider` + `host`; unset = the configured default GitHub
  forge, DL-202), and the substrate resolves it per-provider end to end
  (`forgeProviderRegistry.resolve`, `go/server/forge.go`). Wave parity needs
  both forges — the wave files Linear issues AND opens GitHub PRs — so both
  fields are exposed (Matt's ruling, overruling the earlier single-forge-v1
  deferral). Omitted = GitHub. `forge_provider: "linear"` targets the built,
  unit-tested Linear write provider (`go/internal/forge/linear.go`), which is
  ISSUES-ONLY (DL-051): `repo` is the Linear TEAM KEY (e.g. `"SEA"`), not
  `<owner>/<name>`, and the PR/review arms return in-band `unimplemented`
  (`ErrUnsupported` → `unimplemented`, DL-203). `forge_host` omitted resolves
  the provider's default host (A3); an unknown/unconfigured coordinate is an
  in-band `not_found`, never a wrong-repo write. The tool maps the string
  enum to the generated `ForgeProvider` and sets `forge` only when populated,
  so an omitted selector is a nil `ForgeRef` on the wire.

### Result rendering — refs verbatim-guarded, bodies fenced

Two classes of result, two renderers, both under the established
render-guard discipline:

- **Write acks** (`CommentRef`, `ReviewRef`, and the create arms' returned
  `Issue`/`PullRequest`): a single renderer-authored line —
  `Created issue #<number> in <repo>: <url>` / `Commented on PR #<n>: <url>` /
  `Submitted <verdict> review on PR #<n>: <url>`. Numbers and the verdict pass
  through `attr` (they are in its `[\w.:-]+` class); `url` and `repo` do NOT —
  `attr`'s shape class excludes `/`, so it rejects every well-formed
  `https://…` URL and every `<owner>/<name>` repo as `(malformed)`
  (`render-guard.ts` `attr`, `/^[\w.:-]+$/`). They go through a dedicated
  shape guard sized for URLs and `owner/name` slugs — a `render-guard.ts`
  sibling of `attr` keeping the constrain-don't-escape doctrine (a value
  failing the shape degrades to `(malformed)`, never escapes) — NOT `attr`,
  NOT the fence. **Dedup-hit branch (DL-206):** a replayed create returns a
  skeletal `Issue`/`PullRequest` carrying only `{forge, repo, number}` with
  `url`/`title` empty (`go/server/forge.go` `coordinateResult`), so the create
  renderer branches on an empty `url` and renders `Issue #<n> in <repo>
  already created by an earlier attempt` — the whole-chain dedup working as
  designed must not read as a tool malfunction.
- **Reads** (`Issue`, `PullRequest`, `ListIssuesResponse`): forge bodies and
  titles are member-authored external text — strictly less trusted than
  Compass channel messages — so the comms `comms_list_messages` nonce-fence
  discipline applies verbatim (the comms record's shipped-errata renderer:
  per-render `crypto.randomUUID().slice(0, 8)` fence, one fenced record per
  artifact, `attr`-guarded attributes, a framing line marking bodies as data).
  An issue record carries number, title, state, url, labels, author
  attribution, and the body inside the fence; a PR record adds head/base refs,
  draft, checks roll-up state, and review verdicts. Three read-specific rules
  the comms surface never needed:
  - **Truncation.** Reads have no server page cap the way the comms list does:
    a `PullRequest` carries its full `reviews[]` + `threads[]` with every
    comment body (`compass.proto`), and `ListIssuesResponse` up to `limit`
    (≤100) full bodies. Each rendered body is truncated to a fixed budget
    (a `…(truncated, N chars)` marker inside the fence) and per-artifact
    aggregates (comments/threads) are capped with a count of the elided
    remainder, so a 300-comment PR cannot flood the transcript.
  - **Verdict vocabulary.** The wire echoes the forge's review state
    (`changes_requested`, `compass.proto`) while the tool schema's enum is
    `request_changes`; the renderer normalizes onto the tool vocabulary so the
    model is never shown a verdict string its own schema rejects.
  - **Attribution is parsed, not authenticated.** For external artifacts the
    `agent.agentHandle` attribution is body-PARSED at translation
    (`go/server/forge.go` `translateIssue`), so a non-Compass human who writes
    the owner-header grammar into a GitHub body would render as
    Compass-attributed; the framing line marks attribution on external
    artifacts as a parsed claim, not an authenticated identity.
  `forge.ts` reuses the shared `render-guard.ts` `attr`/`flat`, adds the one
  URL/repo shape guard above (its only new guard), and mints its fence with
  the established idiom.

### Errors — in-band `ForgeCallError` → thrown tool failure, with retry hint

`forgeFailure(result, toolName, expected)` mirrors
`lifecycleFailure`/`commsFailure`: the `error` arm throws
`` `${toolName} failed: ${attr(code)}: ${flat(message).slice(0, 500)}` ``,
any other unexpected arm throws a protocol violation. One forge-specific
addition: `ForgeCallError.retry_after_ms` (the field comms lacks — "a forge,
unlike the in-process comms handler, rate-limits", agent_gateway.proto) is
appended as `` `; retry after ${n}ms` `` when non-zero, so the model can back
off deliberately instead of hammering a rate-limited forge. **This suffix is
future-proofing, not a live signal today:** `mapForgeError`
(`go/server/forge.go`) currently always sets `retry_after_ms` to 0 — neither
`forge.ErrBudgetExhausted` nor `forge.StatusError` surfaces a forge
`Retry-After` yet — so the branch is present and tested but dormant until the
substrate populates it. Unimplemented arms the wire can still express
(subscribe/unsubscribe, or an op a provider cannot serve — DL-202's
`ErrUnsupported` mapping) arrive as ordinary in-band `unimplemented` errors;
no tool exists to send the former, and the latter renders like any tool
failure. Distinct from all of these: a deployment with **no `ForgeCaller`
wired** fails at the relay as a Connect `CodeUnavailable`
(`go/internal/runnerhub/relay_forge.go`), which the gateway forwards and the
transport surfaces as a thrown `ConnectError` — it bypasses `forgeFailure`
(there is no in-band `error` arm to map) but still lands as an ordinary thrown
tool failure, no transport teardown.

### Identity / authz — no new authz CODE, but a new reachable surface

Forge calls attribute to the agent's account via the same session→account
Server-side binding as comms/lifecycle: the Runner structurally owns which
container (hence which 1:1 session) a call arrived on, forwards
`session_id` on `RelayForgeCall`, and the Server resolves the account and
stamps the DL-050 owner header itself under the F1 author/reviewer credential
roles (`buildForgeWriteService` validates both `server_only` secrets at
startup; DL-052 — the write credential never enters the container). The agent
presents no token; a no-`ForgeCaller` deployment fails closed at the relay
(`CodeUnavailable`, surfaced as a thrown tool error per the Errors section),
never a transport teardown. This record writes no authz code and introduces
no credential.

**What IS new is the reachable surface, and the record states it plainly.**
"Nothing new" is true of code, not of exposure. The substrate ships NO scope
rejection (`go/server/forge.go` trust-model: per Resolved decision 2, MVP
single-trust-domain, the caller is recorded for attribution but no scope
rejection ships — A8), and `repo` does not enter the credential key ("one
credential pair serves every repo on a coordinate", `forgeCoordinate`). This
record is the first to hand a MODEL a free-text `repo` over that org-wide
credential: a hallucinated or prompt-injected `repo` writes a real issue/PR
into any repository the shared credential can reach, with DL-050 attribution
as the only audit trail. Unlike comms (which enforces channel membership),
forge enforces nothing — the containment here is prompt-level, not
authz-level. The mitigation is one per-write prompt-guidance line ("operate
only on the repositories your task names") plus the DL-050 attribution trail;
hard scope enforcement is A8's frozen deferral, not reopened here.

## Alternatives considered

- **Fewer, multiplexed tools (one `forge_call` with an `op` parameter, or
  read/write pairs).** Rejected by Matt's ruling: single-purpose tools with
  their own schemas and descriptions are what agents drive reliably; a
  multiplexed tool trades ten precise JSON Schemas for one union schema the
  model must navigate free-form. Also breaks the comms/lifecycle
  one-tool-per-verb house precedent for no token savings that matters (tool
  schemas are sent once per session).
- **Deferring `forge_subscribe`/`forge_unsubscribe` until the writer lands.**
  Considered and OVERRULED by Matt: build the complete tool surface now even
  though the `subscribe`/`unsubscribe` arms return `CodeUnimplemented` until
  the poll-driver lane lands the `agent_forge_subscriptions` writer
  (`go/server/forge.go` — the two arms are `CodeUnimplemented` stubs today).
  A stable, complete surface is worth more than hiding two arms: the tool set
  never changes shape when the writer lands (no re-registration, no prompt
  churn), and the runtime error is honest — the per-tool guidance states the
  arm is not yet wired, so the model learns "not available yet", not "broken".
  This composes with the poll-driver lane
  ([`compass-forge-poll-driver/design.md`](../compass-forge-poll-driver/design.md),
  DL-163: the tables land writer-less; that lane brings the writer).
- **Deferring the `ForgeRef` (provider/host) selector.** Considered and
  OVERRULED by Matt: expose it now. The wave needs BOTH forges — it files
  Linear issues AND opens GitHub PRs — and the substrate already resolves a
  per-provider coordinate end to end (`forgeProviderRegistry.resolve`,
  `go/server/forge.go`; the co-equal Linear write provider
  `go/internal/forge/linear.go` is built and unit-tested). An unexposed
  selector would strand a shipped backend. The hallucination cost the earlier
  deferral cited is bounded by shape: an unknown/unconfigured coordinate is an
  in-band `not_found`, never a wrong-repo write, and `host` omitted resolves
  the provider's default (A3).
- **A single shared broker for comms+lifecycle+forge.** Rejected: the
  one-method structural-subset pattern (`CommsTransport` etc.) exists
  precisely so each tool family depends on one transport method and a unit
  test fakes one method; a shared broker re-couples them.
- **Rendering read results as raw JSON.** Rejected: forge bodies are
  untrusted external markdown; the comms record's transcript-forgery analysis
  applies with more force (anyone on the internet can author an issue body).
  The nonce-fence renderer is the established answer; JSON would leave the
  boundary to whatever the model infers about quoting.

## Global Constraints

Every task below inherits these; they are not repeated per task.

- **No new wire surface.** This record adds no proto, no gen, no RPC. The
  `Forge` RPC, its envelopes, and the TS gen exist; `buf` tasks and the
  RIG-1267 gen-fence are untouched (the fence already covers the `ForgeCall*`
  family and `AgentGateway` — `proto/moon.yml` `gen-fence` script — and no
  public tree is regenerated here).
- **Egress seal preserved.** The forge call rides the existing per-container
  Unix socket — a local hop, not a network address. No forge credential,
  token, Server address, or account identity enters the agent container
  (DL-052: the Server holds the sole write credentials as `server_only`
  declared secrets).
- **No new `@connectrpc` imports outside the carve.** `forge()` lands inside
  `packages/compass-agent/src/transport/**`, the one biome
  `noRestrictedImports` override the transport record established; `forge.ts`
  imports only generated types via the `compassv1.ts` barrel, like
  `comms.ts`/`lifecycle.ts`.
- **Owner header is Server-stamped, never agent-written (DL-050).** Tool
  bodies go up bare; per-tool prompt guidance states it; no TS code
  constructs or strips a header.
- **Red→green testing** (`rule://red-green-testing`): each task lands its
  failing tests first (`forge.test.ts` against a fake `ForgeTransport`;
  transport test extension), then the smallest implementation to green.
- **Formatting/lint gates:** biome for TS (`direnv exec . moon run
  compass-agent:lint`), tests via `compass-agent:test`, markdownlint for this
  record (MD013 disabled repo-wide).
- **Frozen-record convention:** this record freezes on merge; later changes
  supersede by citation. It consumes — never re-decides — the frozen
  transport record (DL-015/016/017), the forge write path (DL-200…DL-206),
  DL-049 (sibling `ForgeCall*` family), DL-050/DL-052 (stamping/credentials),
  DL-069/DL-092 (canonical types on the wire), and DL-202 (ForgeRef
  addressing).
- **arktype pin discipline:** `arktype` stays pinned exact to what the SDK
  resolves (the `comms.ts` note); this record adds no new dependency.

## Plan

Two tasks. T1 is the transport member + broker (the seam extension); T2 is
the tool set + wiring and depends on T1's `ForgeBroker`. Both are TS-only,
test-covered against fakes, and land together or stacked (the driver's call —
one PR carrying both, or T1 then T2 via jj-vine, since T2 depends on T1's
broker).

### T1 — Transport: `forge()` on `RunnerTransport` + socket impl + `ForgeBroker`

In `packages/compass-agent/src/transport/index.ts`:

- Add the interface member, mirroring `comms`/`lifecycle` exactly:

  ```ts
  export interface RunnerTransport {
    comms(req: CommsCallRequest): Promise<CommsCallResult>;
    lifecycle(req: LifecycleCallRequest): Promise<LifecycleCallResult>;
    forge(req: ForgeCallRequest): Promise<ForgeCallResult>;
    // …publishSpine / postConversationFrame / control / close unchanged
  }
  ```

  with `ForgeCallRequest`/`ForgeCallResult` type-imported from
  `../gen/compass/v1/agent_gateway_pb` alongside the existing envelope
  imports.
- Add the one-line delegation in `createUnixSocketTransport`:

  ```ts
  forge: (req) => client.forge(req),
  ```

  The generated `AgentGateway` client already carries `forge` (unary); no
  session-manager, runtime, or close-path change.

In a new `packages/compass-agent/src/forge.ts` (broker half; tools are T2):

```ts
/**
 * The one transport method the forge tools consume — a structural subset of
 * `RunnerTransport` (transport/index.ts), so `createUnixSocketTransport()`'s
 * result satisfies it directly while a unit test fakes a single method.
 */
export interface ForgeTransport {
  forge(req: ForgeCallRequest): Promise<ForgeCallResult>;
}

/**
 * A thin adapter over the forge leg of the Runner transport. `call` delegates
 * straight to `transport.forge(req)`; the Connect unary owns correlation and
 * deadlines. Cancellation is not plumbed (the comms.ts precedent): an aborted
 * turn does not cancel an in-flight create — it lands, and the DL-206
 * idempotency key means a re-issue dedupes rather than double-creating.
 */
export class ForgeBroker {
  constructor(transport: ForgeTransport);
  /** The account-safe idempotency key for a create made under `toolCallId`. */
  idempotencyKey(toolCallId: string): string; // `${#nonce}:${toolCallId}`
  call(req: ForgeCallRequest): Promise<ForgeCallResult>;
}
```

The broker carries the same private `#idempotencyNonce = crypto.randomUUID()`
as `CommsBroker`/`LifecycleBroker`, for the same reason (the errata-documented
cross-session tool-call-id collision under the Server's
`(agent_account_id, client_request_id)` unique memo, DL-206).

Barrel exports: `compassv1.ts` re-exports the forge envelope types + arm
request schemas + result value types it does not yet carry
(`ForgeCallRequest`, `ForgeCallRequestSchema`, `ForgeCallResult`,
`CreateIssueRequestSchema` … `SubmitReviewRequestSchema`,
`ReviewCommentInputSchema`, and the canonical `Issue`/`PullRequest` +
`CommentRef`/`ReviewRef` types from their gen leaves), following the existing
grouped-export style. `index.ts` exports `ForgeBroker` and (T2)
`createForgeTools`, mirroring `CommsBroker`/`createCommsTools`.

**Interfaces:** exactly as quoted — `RunnerTransport.forge(req:
ForgeCallRequest): Promise<ForgeCallResult>`; `ForgeTransport`;
`ForgeBroker.call(req: ForgeCallRequest): Promise<ForgeCallResult>`;
`ForgeBroker.idempotencyKey(toolCallId: string): string`.

**Test cycle (red→green):** extend the transport tests with the `forge`
delegation (the fake-client pattern the comms/lifecycle members use); new
`forge.test.ts` broker cases — `call` forwards to a fake `ForgeTransport` and
returns its result; `idempotencyKey` is nonce-prefixed, stable per broker,
distinct across brokers. `direnv exec . moon run compass-agent:test` red
first, then green; biome clean.

### T2 — Agent: `createForgeTools` (ten tools) + `cli.ts` wiring + prompt guidance

In `packages/compass-agent/src/forge.ts` (same file, tool half):

```ts
/**
 * The native forge tool set. Ten tools, one per `ForgeCallRequest` arm. Two
 * of them — `forge_subscribe`/`forge_unsubscribe` — return the server's
 * in-band `unimplemented` until the poll-driver lane lands the
 * `agent_forge_subscriptions` writer; they ship now so the surface is stable
 * (Matt's build-all ruling). Every tool takes an optional forge selector
 * (`provider` + `host`) so a call targets Linear as well as the default
 * GitHub forge. Wired into the container entrypoint by `cli.ts main()`:
 * merged into the session's `customTools` and registered as `#withNatives`
 * natives.
 */
export function createForgeTools(broker: ForgeBroker): AgentTool[];
```

Exported parameter schemas (arktype, one per tool, exported for wire-contract
tests like `postParameters`/`spawnParameters`); the non-blank `.narrow` bound
on every required string, repeated in the description because `.narrow` has no
JSON Schema form:

```ts
// The optional multi-forge selector, spread into EVERY tool below. Unset =
// the configured default GitHub forge (DL-202); `provider` names Linear /
// GitLab / Forgejo; `host` disambiguates two instances of one provider and
// omitted resolves the provider's default (A3). `create(ForgeRefSchema, …)`
// only when the agent sets it, so an unset selector is a nil ForgeRef on the
// wire (existing default-forge callers unchanged).
const forgeSelector = type({
  "forge_provider?": type("'github' | 'linear' | 'gitlab' | 'forgejo'"),
  "forge_host?": type("string"), // omitted = the provider's default host
});
// EVERY tool schema below also spreads `...forgeSelector` (elided in each body
// for brevity — the eight non-subscription sketches show only their arm-specific
// fields). An implementer adds `...forgeSelector,` to all ten object literals.
export const getIssueParameters = type({
  repo: /* non-blank */ "Repository as <owner>/<name> (GitHub) or team key (Linear)",
  issue_number: type("1 <= number.integer"),
});
export const getPullRequestParameters = type({
  repo: /* non-blank */,
  pull_number: type("1 <= number.integer"),
});
export const listIssuesParameters = type({
  repo: /* non-blank */,
  "state?": type("'open' | 'closed' | 'all'"), // omitted = open
  "labels?": type("string[]"),
  "limit?": type("1 <= number.integer <= 100"), // omitted = server default 30
});
export const commentOnIssueParameters = type({
  repo: /* non-blank */,
  issue_number: type("1 <= number.integer"),
  body: /* non-blank */ "Markdown comment body; do NOT include any attribution header — the server stamps it",
});
export const commentOnPullRequestParameters = type({
  repo: /* non-blank */,
  pull_number: type("1 <= number.integer"),
  body: /* non-blank, same stamping note */,
});
export const submitReviewParameters = type({
  repo: /* non-blank */,
  pull_number: type("1 <= number.integer"),
  verdict: type("'approve' | 'request_changes' | 'comment'"),
  "body?": type("string"), // required unless verdict='approve' (see .narrow)
  "comments?": type({
    path: /* non-blank */,
    line: type("1 <= number.integer"),
    "side?": type("'LEFT' | 'RIGHT'"), // omitted = RIGHT
    body: /* non-blank */,
  }).array(),
}).narrow((v, ctx) =>
  v.verdict === "approve" || (v.body?.trim().length ?? 0) > 0
    ? true
    : ctx.reject("body is required unless verdict is 'approve'"));
export const createIssueParameters = type({
  repo: /* non-blank */,
  title: /* non-blank */,
  "body?": /* stamping note; optional — the server stamps a header into an empty body (DL-050) */,
  "labels?": type("string[]"),
});
export const createPullRequestParameters = type({
  repo: /* non-blank */,
  title: /* non-blank */,
  "body?": /* stamping note; optional (DL-050) */,
  head_ref: /* non-blank */ "The branch you already pushed with your git credential",
  "base_ref?": type("string"), // omitted = repo default branch
  "draft?": type("boolean"),
});
// Built now (Matt's build-all ruling) though the arms are CodeUnimplemented
// stubs until the poll-driver lane lands the subscription writer; the tool
// renders the server's in-band `unimplemented` and the description says so.
export const subscribeParameters = type({
  ...forgeSelector,
  repo: /* non-blank */,
  kind: type("'issue' | 'pull_request'"),
  number: type("1 <= number.integer"),
});
export const unsubscribeParameters = type({
  ...forgeSelector,
  subscription_id: /* non-blank */ "The id returned by forge_subscribe",
});
```

Every `/* non-blank */` above is shorthand for the same required-non-blank
idiom spelled out concretely on `submit_review`'s body —
`type("string").narrow((v, ctx) => v.trim().length > 0 ? true : ctx.reject("must not be blank"))` —
with the trailing string literal (where shown) as the arktype description; an
implementer must copy that bound, not the literal `/* non-blank */` comment.

Every schema also spreads `...forgeSelector` (shown once above, elided in each
body for brevity): the optional `forge_provider`/`forge_host` pair is on all
ten tools. When either is set, `execute` builds
`create(ForgeRefSchema, { provider: <mapped enum>, host: forge_host ?? "" })`
and sets `ForgeCallRequest.forge`; when both are unset it leaves `forge` nil
(the default-GitHub path). The string enum maps to the generated
`ForgeProvider` (`'linear'` → `FORGE_PROVIDER_LINEAR`, etc.); an unknown
coordinate is the server's in-band `not_found`, never a wrong-repo write.

Each `execute` builds `create(ForgeCallRequestSchema, { callId: toolCallId,
call: { case: <arm>, value: create(<ArmSchema>, …) }, forge, clientRequestId })`
(`forge` set only when the selector is populated; `clientRequestId` only on the
two create arms, = `broker.idempotencyKey(toolCallId)`), awaits `broker.call`,
checks the expected result arm (else `forgeFailure`), and renders per the
Approach (write acks as one shape-guarded line — `url`/`repo` through the
URL/repo shape guard, numbers/verdict through `attr`; reads through the
nonce-fence renderer; `forge_subscribe` renders its `subscription_id` from the
`subscribed` arm, `forge_unsubscribe` an ack from `unsubscribed` — but until
the writer lands both arms only ever return the `error` arm as `unimplemented`,
so their happy-path render is dormant like the retry hint). The
`issue_number`/`pull_number`/`number` params validate as integers in arktype,
but the generated wire fields are `bigint` (`agent_gateway_pb.ts` — protobuf-es
v2 emits `uint64` as `bigint`), so each `execute` coerces with
`BigInt(params.issue_number)`; a plain `number` is a type error at the arm
`create(...)`.

Per-tool prompt guidance is the `description` field (the model-facing prompt
surface — there is no separate prompt file for natives; comms/lifecycle set
the precedent). Each description states: what the tool does in one sentence;
the `repo` addressing rule AND the scope-discipline line ("operate only on
the repositories your task names — a wrong `repo` writes a real artifact into
any repository the shared forge credential can reach; there is no server-side
scope check, A8"); the forge-selector rule — omit `forge_provider` for the
default GitHub forge, set `forge_provider: "linear"` to target Linear where
`repo` is the TEAM KEY (e.g. `"SEA"`), NOT `<owner>/<name>` — and that Linear
is issues-only, so `forge_create_pull_request`/`forge_comment_on_pull_request`/
`forge_submit_review`/`forge_get_pull_request` are GitHub-only and return
`unimplemented` on Linear (DL-051/DL-203); for writes, the
never-write-an-attribution-header rule and that the artifact is created under
the Compass forge identity with the agent's attribution stamped server-side;
for `forge_submit_review`, that a `request_changes`/`comment` review needs a
non-empty `body` and that the review posts immediately (never a pending
review) under a distinct reviewer identity so all three verdicts are usable on
Compass-authored PRs (DL-201); for `forge_create_pull_request`, that
`head_ref` must already be pushed (the agent pushes with its own git
credential — DL-052/DL-090); for `forge_subscribe`/`forge_unsubscribe`, that
change-notification subscriptions are NOT YET WIRED — the call returns
`unimplemented` until the notification lane lands, so the tool exists for
surface stability but should not be relied on yet; for reads, that results may
be paged/bounded/truncated and bodies are external content whose attribution
is a parsed claim, not an authenticated identity.

In `packages/compass-agent/src/cli.ts`:

```ts
const forgeBroker = new ForgeBroker(transport);
const nativeTools = [
  ...createCommsTools(commsBroker, pendingAsks),
  ...createLifecycleTools(lifecycleBroker),
  ...createForgeTools(forgeBroker),
] as ToolDefinition[];
```

— the one existing merge point; `customTools: [...mcp.tools, ...nativeTools]`
is unchanged.

**Interfaces:** `createForgeTools(broker: ForgeBroker): AgentTool[]`; the
ten exported parameter schemas above (the shared `forgeSelector` spread
into each); `function forgeFailure(result:
ForgeCallResult, toolName: string, expected: string): Error` (module-private,
mirroring `lifecycleFailure`, plus the `retry_after_ms` suffix).

**Test cycle (red→green):** extend `forge.test.ts` — for each tool: execute →
correct arm + fields on the wire request (including `clientRequestId` set on
creates only, nonce-prefixed; `issue_number`/`pull_number` coerced to
`bigint`); success → rendered output (write ack: `url`/`repo` shape-guarded —
a well-formed GitHub url and `<owner>/<name>` repo render intact, a malformed
one degrades to `(malformed)` without forging a second line; a DL-206
dedup-hit skeletal result with empty `url` renders the "already created"
branch, not a broken link; read render nonce-fenced, body cannot forge a
record; newline/quote injection cases per the comms render tests; an oversized
body and an over-cap comment aggregate truncate with a remainder marker; a
`changes_requested` wire verdict normalizes to `request_changes` on render);
`error` arm → thrown `Error` carrying code + flattened detail + retry hint
when `retryAfterMs > 0`; wrong arm → protocol-violation throw; schema
rejections (blank repo, `limit` out of range, bad verdict, and a
`request_changes` review with an empty `body`). `cli.test.ts` extension: the
forge tools appear in the session's `customTools` (the existing
natives-wiring assertion pattern). `direnv exec . moon run
compass-agent:test` + `compass-agent:lint` green.

## Tasks

- [ ] T1 — Transport + broker: `forge()` member on `RunnerTransport` +
  one-line `createUnixSocketTransport` delegation; `ForgeTransport` +
  `ForgeBroker` (nonce-scoped `idempotencyKey`) in new `forge.ts`;
  `compassv1.ts` barrel exports for the forge envelopes/arm schemas/canonical
  result types; transport + broker tests red→green; biome clean.
- [ ] T2 — Tools + wiring: `createForgeTools` with the ten tools (exact
  schemas above; creates carry the DL-206 key and coerce numbers to `bigint`;
  the optional `forge` selector — `provider` enum + optional `host` — built on
  every tool, unset = default GitHub, `create(ForgeRefSchema,…)` only when set;
  the `url`/`repo` shape guard added to `render-guard.ts`; write acks through
  that guard with the dedup-hit branch; reads nonce-fenced + truncated +
  verdict-normalized; `forge_subscribe`/`forge_unsubscribe` built over the
  `subscribe`/`unsubscribe` arms, rendering the server's in-band
  `unimplemented` cleanly; `forgeFailure` with the dormant retry hint), per-tool
  prompt guidance in descriptions (incl. the repo-scope discipline line, the
  Linear-vs-GitHub `repo`-shape rule, and the PR/review-are-GitHub-only note);
  `cli.ts` `ForgeBroker` + spread into `nativeTools`; `index.ts` exports; the
  forge tool family added to `packages/compass-agent/AGENTS.md` (the package
  contract `comms.ts` defers to); `forge.test.ts` + `cli.test.ts` extensions
  red→green; biome clean.

## Ledger-impact

One proposed DECISIONS.md row (appended by the driver at PR time, wherever
the ledger then lives — compass-repo RIG-2577 T2 may relocate it to
`docs/designs/DECISIONS.md`):

> The agent forge native toolset is ten single-purpose tools, one per
> `ForgeCallRequest` arm (`forge_get_issue`, `forge_get_pull_request`,
> `forge_list_issues`, `forge_comment_on_issue`,
> `forge_comment_on_pull_request`, `forge_submit_review`,
> `forge_create_issue`, `forge_create_pull_request`, `forge_subscribe`,
> `forge_unsubscribe`), each a native `AgentTool` over a thin `ForgeBroker`
> on the `RunnerTransport.forge()` seam; `forge_subscribe`/`forge_unsubscribe`
> ship the complete surface but return the server's in-band `unimplemented`
> until the poll-driver lane lands the `agent_forge_subscriptions` writer
> (DL-163). Multi-forge is exposed: every tool takes an optional forge
> selector (`provider` + optional `host`, unset = the configured default
> GitHub forge, DL-202) so an agent targets Linear (issues-only, `repo` = team
> key, DL-051) as well as GitHub; PR/review arms on a non-GitHub provider
> return in-band `unimplemented`.

This mirrors how DL-212 records the comms toolset; the tool-count claim is
load-bearing for future toolset-refresh rows.
