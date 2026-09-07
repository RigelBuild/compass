# Design: Forge state-transition write op (RIG-3331)

Status: Active

## Problem / Intent

A Compass agent cannot change the state of a forge artifact. The `forgeService`
write arms are create / comment / review, plus get / list / subscribe — no arm
transitions an issue or PR between open and closed, and no `forge.Provider`
method exists for it. Matt ruled (2026-09-05): "we need state transitions 100%.
if they are missing we need to add immediately." This record designs the op:
an agent sets an issue/PR's state on BOTH GitHub and Linear, through the same
attribution chokepoint every other forge write rides — and the emitted STATE
event carries the acting agent's identity, the load-bearing contract the
RIG-3326 self-origin suppression record keys its STATE arm on.

## Approach

### What exists today (grounding)

**The write-arm pipeline.** `createIssue` in `go/server/forge.go` states the
canonical create shape:

> createIssue is a create arm: resolve target, F3 dedup (a hit returns the
> recorded coordinate with ZERO provider calls), then on a miss stamp → author
> client write → flatten → DL-055 row+memo → result.

**Not every write mints a coordinate.** `commentOnIssue` in the same file:

> commentOnIssue stamps the comment body (author client) and returns the write
> ack. A comment mints no artifact coordinate, so it is neither F3-deduped nor
> DL-055-recorded (the store index has no comment kind).

And the `record` helper pins the F3 boundary as a frozen ruling:

> Only create_issue / create_pull_request mint an artifact coordinate (kind
> issue|pull_request); the comment/review arms have no coordinate to record,
> so they never reach here (F3 is create-only per the frozen ruling …).

**The provider seam.** The `Provider` interface in
`go/internal/forge/provider.go` is one method per forge operation, `ctx` +
`repo` first, an input struct for compound writes, the raw forge value type
back:

```go
type Provider interface {
    Name() string
    CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error)
    CommentOnIssue(ctx context.Context, repo string, number uint64, body string) (Comment, error)
    GetIssue(ctx context.Context, repo string, number uint64) (Issue, error)
    ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error)
    CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error)
    CommentOnPullRequest(ctx context.Context, repo string, number uint64, body string) (Comment, error)
    SubmitReview(ctx context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error)
    GetPullRequest(ctx context.Context, repo string, number uint64) (PullRequest, error)
    Checks(ctx context.Context, repo string, number uint64) (Checks, error)
    BodyLimit() int
}
```

An unservable operation has a sentinel, `ErrUnsupported` in the same file:

> ErrUnsupported is returned by a provider for an operation it cannot serve
> (e.g. an issues-only forge for the PR half). #995 Decision 3.

**The state vocabulary.** The raw `forge.Issue` type in
`go/internal/forge/provider.go` fixes the portable domain:

> State is the forge's truth ("open" | "closed"), not the Compass lifecycle.

The PR side adds one value: `toPullRequest` in `go/internal/forge/github.go`
folds GitHub's separate merged bool —

> State folds the merged bool: merged==true -> "merged", else the raw
> open|closed (so State stays in the domain's {open,closed,merged}).

Linear is already collapsed onto the same truth: `linearClosedStateTypes` in
`go/internal/forge/linear.go` —

> Every other type maps to "open". Verified against Linear SDL
> WorkflowState.type: "triage", "backlog", "unstarted", "started", "completed",
> "canceled", "duplicate".

```go
var linearClosedStateTypes = []string{"completed", "canceled"}
```

**How COMMENT/REVIEW carry the actor.** `CommentRef` in
`proto/compass/v1/forge.proto`:

```proto
message CommentRef {
  string url = 1;                        // the forge comment permalink
  uint64 comment_id = 2;                 // the forge comment id
  string body = 3;                       // the comment text; unset on a write ack
  string forge_account = 4;              // the commenter's forge login; always set on a notification
  compass.v1.AgentAttribution agent = 5; // set only when the commenter is a Compass agent; unset for a human
  string comment_key = 6;                // …
}
```

with `AgentAttribution` in `proto/compass/v1/compass.proto`:

```proto
message AgentAttribution {
  string agent_handle = 1;  // the authoring agent's handle, from the header
}
```

The attribution's *source* is the stamped owner header in the comment body:
`translateAttribution` sites such as the one in
`go/internal/ingest/notify_detect.go` build
`ref.Agent = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}` from
the parsed header. This is the mechanism a state transition CANNOT reuse — a
close/reopen carries no body to stamp.

**What a STATE event carries today: no actor at all.** The GitHub webhook arm
(`gitHubStateOrUpdateKind` in `go/internal/forge/githubapp_webhook.go`) maps

> closed/reopened->STATE

and sets only `base.State = wh.Issue.State`. The Linear data-change arm
(`ParseLinearDataEvent` in `go/internal/linearagent/data_event.go`) maps

> Issue update->STATE iff updatedFrom shows a workflow-state change

and sets `base.State = forge.MapLinearState(de.Data.State.Type)`. Neither
parses an actor for STATE. The router's `notification` builder in
`go/internal/ingest/notify_router.go` confirms the wire payload is bare:

```go
case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE:
    n.State = ev.State
```

and `ForgeNotification.state` in `proto/compass/v1/forge.proto` is

> Set for STATE: the new forge state string ("closed", "merged", …).

So every STATE event reaching the router today is actor-less: an agent's own
transition and a human's close of the agent's issue are indistinguishable.
That is the gap this op must close (the RIG-3326 STATE suppression arm must
key on the REAL transition actor, never the DL-055 author-row proxy — the
proxy would wrongly suppress a human-close notification to the issue's
author).

### The cross-provider state model

The two providers disagree structurally:

- **GitHub**: issue/PR state is a two-value enum, open|closed, mutated by
  `PATCH /repos/{owner}/{repo}/issues/{number}` with `state`, plus an optional
  close *reason* (`state_reason`: completed | not_planned) on issues; PRs take
  `PATCH /repos/{owner}/{repo}/pulls/{number}` with `state` only. Merge is a
  separate operation and is NOT this op's business (the domain already treats
  "merged" as a read-side fold, per the `toPullRequest` quote above).
- **Linear**: an issue's state is a reference to a per-team, user-named
  workflow state (an `issueUpdate` mutation with a target `stateId`). There is
  no fixed enum of states — only a fixed enum of state *types* (the SDL
  `WorkflowState.type` list quoted above). Two teams can both have a state
  named "Done" with different ids; a team may have several states of type
  "completed".

The request therefore carries a **portable core plus optional per-provider
refinements**, mirroring how the read side already collapses both providers
onto one truth:

- `state` — REQUIRED, the portable target: `open` | `closed`. Exactly the
  `forge.Issue.State` domain. This is the only field a cross-provider caller
  needs, and it is expressible on both providers.
- `close_reason` — OPTIONAL, GitHub-issue refinement: `completed` |
  `not_planned`. Empty means the provider default.
- `workflow_state` — OPTIONAL, Linear refinement: the target workflow state's
  NAME, resolved per-team (name → id, cached the way `resolveTeamID` already
  caches team key → team UUID: "teamIDs caches Linear team key -> team UUID; a
  key is resolved once via a teams query and reused"). Empty means the
  provider maps the portable `state` to a default workflow state (rule below).

**When a caller asks for a state one provider cannot express**, the arm fails
loud, in-band, BEFORE any provider call: a refinement set for the wrong
provider (`workflow_state` on GitHub, `close_reason` on Linear or on a PR) is
an in-band `invalid_argument` ForgeCallError naming the field and provider —
the `resolveTarget` posture ("empty is invalid_argument BEFORE any store or
provider touch") extended to refinement/provider mismatch. Silently dropping
the refinement is rejected: the caller asked for something specific and the
write would not do it. A `workflow_state` name that does not exist on the
team, and a consistency violation (`workflow_state` naming a state whose type
contradicts `state` — e.g. `state: closed` with a "started"-type name), are
likewise in-band `invalid_argument` from the Linear provider, before the
mutation. A PR transition on Linear is `ErrUnsupported` (Linear has no PRs —
the "issues-only forge for the PR half" case the sentinel was minted for),
flattened by `mapForgeError` to `unimplemented` naming provider+op:

> ErrUnsupported → unimplemented naming provider+op

Reopening a merged GitHub PR is whatever the forge says it is — GitHub answers
422, and the existing flattening already carries it:

> \*StatusError{422} → invalid_argument carrying the forge's validation message

**Linear default-state resolution (when `workflow_state` is empty):**
`state: closed` targets the team's lowest-positioned workflow state of type
`completed`; `state: open` targets the team's lowest-positioned state of type
`unstarted` (falling back to `backlog` if the team has no unstarted state).
This inverts the read-side mapping (`linearClosedStateTypes` = completed,
canceled ⇒ closed; everything else ⇒ open) with one deliberate asymmetry:
default-close picks `completed`, never `canceled` — an agent closing its issue
means "done", and "canceled" is reachable explicitly via `workflow_state`.
The choice of default rule is surfaced as OQ-2 (it picks a human-visible
board column on the caller's behalf).

### The wire shape

A new `ForgeCallRequest` oneof arm pair, following the issue/PR parallel-arm
convention (`comment_on_issue` / `comment_on_pull_request` are deliberately
parallel arms, per the `//nolint:dupl` notes on both). The proto file already
has a board-lane `SetIssueStateRequest` (the `BoardCallRequest` arm operating
on the Compass-local `Issue.id` + `compass.v1.IssueState`), so the forge
messages take the *transition* stem — the issue's own title vocabulary — to
avoid colliding with that existing name:

```proto
// agent_gateway.proto — ForgeCallRequest gains (next free arm numbers after
// the documented "call_id=1, oneof arms 2-11, forge=12 … client_request_id=13"
// layout; 14 and 15 are collision-free):
oneof call {
  // … existing arms 2-11 …
  TransitionIssueStateRequest transition_issue_state = 14;
  TransitionPullRequestStateRequest transition_pull_request_state = 15;
}

message TransitionIssueStateRequest {
  string repo = 1;            // REQUIRED; "<owner>/<name>" on GitHub, team key on Linear
  uint64 issue_number = 2;
  string state = 3;           // REQUIRED: "open" | "closed" (the forge.Issue.State domain)
  string close_reason = 4;    // GitHub only: "completed" | "not_planned"; "" = provider default
  string workflow_state = 5;  // Linear only: target workflow state NAME; "" = default mapping
}

message TransitionPullRequestStateRequest {
  string repo = 1;
  uint64 pr_number = 2;
  string state = 3;           // "open" | "closed"; merge is a separate concern, never expressed here
}
```

The result arms are REUSED, not new: a successful transition returns the
updated artifact on the existing `ForgeCallResult.issue` / `pull_request`
arms (the arms `create_issue / get_issue` already share), so the caller sees
the post-transition truth the same way a create's caller sees the created
artifact. `state` is a string, not a new enum — it is the same raw forge
state string the whole read path already speaks (`forge.Issue.State`,
`ForgeNotification.state`), and DL-069's no-forge-shape-on-the-wire rule
concerns message *types*, which this adds none of.

### The provider methods

Two new `Provider` methods, matching the signature style exactly (`ctx`,
`repo`, `number`, input struct for the compound write, raw value type back):

```go
// TransitionState is the input to Provider.TransitionIssueState /
// TransitionPullRequestState. State is the portable target ("open"|"closed");
// CloseReason and WorkflowState are the per-provider refinements — a provider
// receiving a refinement it cannot express has already been screened at the
// server arm, so it may ignore the foreign field.
type TransitionState struct {
    State         string // "open" | "closed"
    CloseReason   string // GitHub issues: "completed" | "not_planned"; "" = default
    WorkflowState string // Linear: target workflow state name; "" = default mapping
}

// Provider gains:
TransitionIssueState(ctx context.Context, repo string, number uint64, in TransitionState) (Issue, error)
TransitionPullRequestState(ctx context.Context, repo string, number uint64, in TransitionState) (PullRequest, error)
```

- **GitHub**: `TransitionIssueState` = `PATCH /repos/{repo}/issues/{number}`
  with `state` (+ `state_reason` when closing with a reason);
  `TransitionPullRequestState` = `PATCH /repos/{repo}/pulls/{number}` with
  `state`. Both re-read nothing: the PATCH response is the updated artifact,
  decoded through the existing `ghIssue` / `ghPullDetail` wire structs.
- **Linear**: `TransitionIssueState` resolves the team (existing `teamIDs`
  cache), resolves the target workflow state id — by name when
  `workflow_state` is set, by the default rule otherwise — via a per-team
  workflow-states query cached beside `teamIDs`, then runs an `issueUpdate`
  GraphQL mutation with the target `stateId`, returning the updated issue
  through the existing `issueFieldsFragment` decode.
  `TransitionPullRequestState` returns `ErrUnsupported`.
- **Fake**: `fake.go` gains both methods with scriptable results/errors, as
  every other operation has.

### The server arm: what F3 and DL-055 mean for a transition

A state transition **mutates an existing coordinate and mints none**. From the
code read above, both create-only mechanisms are inapplicable, and the comment
arm is the precedent, not the create arm:

- **No F3 dedup.** F3 is "create-only per the frozen ruling" (the `record` doc
  comment quoted above), and its memo returns *the recorded coordinate* — a
  transition creates no coordinate to record or return. `client_request_id` is
  already documented as "Ignored on non-create arms" on `ForgeCallRequest`;
  the transition arms inherit that. The idempotency story is the operation's
  own semantics: closing a closed issue is a no-op PATCH on GitHub and a
  same-state `issueUpdate` on Linear — a retried transition converges on the
  target state rather than duplicating anything, which is exactly the hazard
  class F3 exists to prevent on creates and which transitions do not have.
- **No DL-055 row.** The DL-055 ownership index records *who authored an
  artifact* ("Only create_issue / create_pull_request mint an artifact
  coordinate"); a transition changes no authorship and may act on an artifact
  the agent never authored (no row exists at the coordinate at all). Writing
  or touching the author row would corrupt its meaning — and keying anything
  off it is precisely the author-row-proxy failure RIG-3326 rejects.
- **No body, so no stamp and no body limit.** The DL-050 stamp chokepoint
  attributes *bodies*; a transition has none. Attribution rides the mechanism
  in the next section instead.

The arm's pipeline is therefore: resolve target → validate (portable state in
domain; refinements match the resolved provider; consistency) → author client
write → flatten (`mapForgeError`, unchanged) → **transition memo write** →
result. The memo write is the transition's analogue of the create's
"DL-055 row+memo" step — strictly after forge success, so a rejected write
leaves no memo (mirroring `record`'s "strictly AFTER a create's forge
success" ordering).

### Actor attribution: the load-bearing contract

**The contract (what RIG-3326 keys on):** after a successful agent-driven
state transition, the STATE event the notify pipeline emits for that
transition resolves to the acting agent's identity — the agent that made the
forge call, as resolved by `resolveIdentity` at the chokepoint ("the agent's
own handle (AgentHandle), the owning user's handle (OwnerHandle)") — and a
STATE event NOT caused by an agent-driven transition resolves to no actor.
Suppression then fires only on the real transition actor; a human closing an
agent's issue matches nothing and is delivered.

**The mechanism (recommended): a consumable transition memo.** The write
chokepoint records, in the same post-success step as above, one row per
coordinate in a new store table (`forge_state_transitions`): provider, host,
repo, kind, number, the applied portable state, the acting agent's account id,
and a written-at timestamp — upserted, latest transition wins. When the STATE
event for that transition echoes back through the provider (the GitHub webhook
`closed`/`reopened` arm; the Linear data-change `updatedFrom.stateId` arm) and
reaches the router, the router resolves the event's actor through a
package-local seam (the `NotifyStore` / `ChecksRoller` shape — the `ingest`
package "deliberately never imports the store", so the store enters through a
go/server-adapted interface): a point read at the coordinate that matches the
memo's applied state against the event's state within a freshness bound, and
CONSUMES the memo in the same statement (an `UPDATE … RETURNING`-style
one-shot, so one memo attributes at most one event). A miss — no memo, stale
memo, state mismatch — resolves no actor, which is the correct answer for
every human/external transition and the safe answer for every race
(fail-open: an unattributed self-transition costs one redundant wake, never a
lost cross-agent signal).

Why not stamp the actor into the event at the source parser: the webhook
payload's actor is the forge login (the `whUser` "actor sub-object GitHub
attaches to comments and artifacts"), and every Server-credential write
presents the shared App bot login — it identifies the chokepoint, never which
agent called it. The comment path solves this with the in-body owner header;
a transition has no body, so durable server-side correlation is the only
channel that can carry the agent's identity across the write→webhook gap.

The alternative — the chokepoint synthesizing the STATE `ForgeEvent` itself,
actor attached, at write time — is weighed in Alternatives considered and
rejected for the double-notification and lane-coupling problems; the fork is
OQ-1 because RIG-3326's frozen text names an event-carried actor.

This record deliberately freezes the *durable half* (memo write at the
chokepoint + the consume-on-match store read) and the *contract* the router
consumes; the router-side match/suppress logic is RIG-3326's own scope (PR
open at Matt's gate). Nothing here depends on that PR landing first — the memo
and its resolver seam are correct and testable standalone, and the consumer
binds to the contract stated above whenever it lands.

### Agent tools

DL-241 froze the toolset as arm-mirroring:

> The agent forge native toolset is ten single-purpose tools, one per
> `ForgeCallRequest` arm …

Two new arms ⇒ two new tools, `forge_transition_issue_state` and
`forge_transition_pull_request_state`, same thin-broker pattern. The DL-241
row's "ten" count is amended by citation in this record's ledger row (rows are
append-only; DL-241 stays Active — the one-tool-per-arm *rule* is unchanged,
and this record follows it).

### Test tiers

Per DL-210 in `docs/designs/DECISIONS.md`, two tiers only:

> Forge integration testing adds two live-contract tiers above the DL-174
> hermetic pyramid: (1) a hermetic golden-fixture replay leg (committed
> `go/internal/forge/testdata/` fixtures replayed through the stub
> RoundTripper …) and (2) a `//go:build livegithub` live-credentials oracle
> (same scenarios against a throwaway `RigelBuild/compass-forge-testbed` + a
> Linear …)

Both new provider methods get golden-replay fixtures (GitHub issue close with
reason / reopen / PR close / PR reopen; Linear close-by-default / close-by-name
/ reopen / unknown-name rejection) and matching `livegithub` oracle legs
against the testbed. Server-arm logic (validation, memo ordering, fake-backed
pipeline) rides the untagged unit tier with the fake provider, like every
existing arm.

## Alternatives considered

### Synthetic STATE event at the write chokepoint (vs the memo)

The chokepoint could construct a `forge.ForgeEvent` (STATE, actor attached)
and inject it into the notify lane directly at write success. Rejected as the
recommendation: (a) the provider's own webhook echo for the same transition
still arrives, producing a second STATE route for one transition — the
synthetic path would need webhook-echo dedup, a mechanism nothing in the
pipeline has today; (b) it couples `forgeService` to the notify lane's router,
a dependency direction that does not exist (the lanes are wired independently
at serve assembly); (c) delivery timing gains nothing — the memo path
attributes the SAME webhook-driven event the pipeline already routes. The memo
is one table + one seam, entirely inside existing patterns. Kept as OQ-1
because the consumer record's frozen text speaks of the actor "stamped … onto
the emitted event", which reads closer to the synthetic shape.

### A portable state enum on the wire (vs the raw string)

A new proto enum (`OPEN`/`CLOSED`) would be self-documenting but would mint a
second state vocabulary beside the raw forge state string the entire read path
already carries (`forge.Issue.State`, `ForgeNotification.state`, the
`ListIssuesRequest.state` filter — all strings). One vocabulary wins; the arm
validates the string against the two-value domain at the edge.

### Single kind-discriminated arm (vs the issue/PR pair)

One `TransitionArtifactStateRequest` with a `ForgeArtifactKind` field would
halve the proto surface but break the established parallel-arm convention
(every existing write is per-kind: create, comment ×2) and force the
kind-unspecified rejection into every handler. The pair matches the codebase.

## Global Constraints

- Go toolchain per repo convention; build tags `-tags unix`; never `-race`;
  no `time.Sleep` in tests.
- Two forge test tiers only (DL-210): hermetic golden replay (untagged) +
  `//go:build livegithub` live oracle. No third harness.
- Proto arm numbers are frozen once assigned: `transition_issue_state = 14`,
  `transition_pull_request_state = 15` (first free after the documented
  `call_id=1, arms 2-11, forge=12, client_request_id=13` layout).
- The portable state domain is exactly `{open, closed}` on requests. `merged`
  is read-side only (the `toPullRequest` fold); no request may express it.
- No forge domain type on the wire (DL-069): requests stay all-scalar, results
  reuse the canonical `compass.v1.Issue` / `PullRequest` arms.
- Refinement/provider mismatch fails in-band `invalid_argument` BEFORE any
  provider call; never silently dropped.
- The transition memo is written strictly AFTER forge success (the `record`
  ordering), and consumed at most once per transition.
- Attribution never rides forge text: the memo correlates by server-side
  coordinate + state + freshness, never by parsing anything a forge user could
  have written (DL-050/DL-094 posture).

## Plan

Ordering: T0 → T1 → {T2, T3} → T4 → {T5, T6} → T7 → T8. T2 and T3 are
independent of each other; T5 and T6 both depend only on T4.

### T0 — Proto: request arms + regen

The two `ForgeCallRequest` arms and request messages exactly as in §The wire
shape; the `ForgeCallResult.issue` / `pull_request` arm comments gain the
transition ops to their served-by lists. Regenerate.

Interfaces: consumes nothing; produces the generated
`TransitionIssueStateRequest` / `TransitionPullRequestStateRequest` Go types
and the two new `ForgeCallRequest_TransitionIssueState` /
`_TransitionPullRequestState` oneof wrappers.

Tests: none beyond regen compiling (proto-only slice); the arm dispatch test
lands in T4.

### T1 — Provider interface + fake

Add `TransitionState` and the two methods to `Provider`
(`go/internal/forge/provider.go`) per §The provider methods; extend `fake.go`
with scriptable implementations (result + error injection, mirroring the
existing per-method fake shape).

Interfaces: produces
`TransitionIssueState(ctx, repo string, number uint64, in TransitionState) (Issue, error)`
and
`TransitionPullRequestState(ctx, repo string, number uint64, in TransitionState) (PullRequest, error)`
on `forge.Provider`, consumed by T2/T3/T4.

Tests: fake round-trip in the existing `fake_test.go` style.

### T2 — GitHub implementation + fixtures

`PATCH`-based implementations per §The provider methods, decoding through the
existing `ghIssue` / `ghPullDetail` structs (PR state folds merged exactly as
`toPullRequest` already does). Rate-gate and conditional-request behavior
identical to the other write methods (writes are unconditional; the fail-fast
budget gate applies).

Interfaces: consumes T1's signatures; produces golden fixtures under
`go/internal/forge/testdata/` for close-with-reason / close-default / reopen /
PR close / PR reopen / 422-on-merged-PR-reopen.

Tests: golden replay (untagged) + `livegithub` legs against the testbed
(close→verify state via `GetIssue`→reopen; PR twin).

### T3 — Linear implementation + fixtures

Workflow-state resolution (per-team name→id + type, cached beside `teamIDs`
with the same mutex discipline), the default-mapping rule from §The
cross-provider state model, the consistency check (named state's type must
agree with the portable target), the `issueUpdate` mutation, and
`ErrUnsupported` on the PR method.

Interfaces: consumes T1; produces fixtures for close-by-default /
close-by-name / reopen-by-default / unknown-name (`invalid_argument`) /
type-contradiction (`invalid_argument`).

Tests: golden replay + `livegithub` Linear legs (gated on the existing
`LINEAR_FORGE` app-actor token per DL-324).

### T4 — Server arms + transition memo

The two `forgeService` arms per §The server arm: `resolveTarget`, then arm
validation (state domain; refinement/provider screen; PR-refinement screen),
author-client dispatch, `mapForgeError` flattening, memo write on success,
updated canonical artifact on the result arm (through the existing
`translateIssue` / `translatePR` helpers). Store side: the
`forge_state_transitions` table (migration), an upsert write + a
consume-on-match read on `*store.Store`, and the `forgeStore` narrow-interface
widening so the ordering is provable against the fake store.

Interfaces: consumes T0's generated types + T1's provider methods; produces
the memo store surface
`RecordStateTransition(ctx, …coordinate…, state string, agent AccountID, at time.Time) error`
and
`ConsumeStateTransition(ctx, …coordinate…, state string, fresh time.Time) (AccountID, bool, error)`
(exact SQL shapes at execution; the consume is a single-statement
clear-and-return).

Tests: unit (fake provider + fake store): dispatch, validation rejections
(each screen), memo written only after success, memo absent after provider
failure; pgtest for the store surface (upsert-latest-wins, consume-once,
freshness bound, miss cases).

### T5 — STATE actor resolution seam (the RIG-3326 contract surface)

The go/server-adapted seam through which the notify lane resolves a STATE
event's actor from the memo: an `ingest`-package-local interface (the
`NotifyStore` no-store-rule shape) backed by `ConsumeStateTransition` +
account→handle resolution, wired into both notify lane builders. This task
delivers the seam and its adapter; whether the router *uses* it to suppress is
RIG-3326's implementation, built against the contract in §Actor attribution.

Interfaces: consumes T4's store surface; produces the seam (final name/shape
coordinated with the RIG-3326 executor at execution — the contract, not the
identifier, is frozen here) returning the acting agent's owner-qualified
identity or a clean miss.

Tests: pgtest — a recorded transition resolves for a matching STATE event
exactly once; a second identical event resolves nothing; a human transition
(no memo) resolves nothing; a stale memo resolves nothing.

### T6 — Agent tools

`forge_transition_issue_state` + `forge_transition_pull_request_state`, the
DL-241 single-purpose-tool pattern over the same `ForgeBroker` seam.

Interfaces: consumes T0's arms; produces the two tool definitions + broker
plumbing.

Tests: the existing tool-surface test conventions (schema render + broker
round-trip against a scripted result).

### T7 — Live-oracle end-to-end sweep

The two-tier closure per DL-210: confirm every T2/T3 fixture has a matching
`livegithub` leg, and add the cross-op oracle scenario (create → transition →
`GetIssue` state assertion → reopen) on both providers.

Interfaces: consumes T2/T3.

### T8 — Ledger append

Append the rows from §Ledger impact to `docs/designs/DECISIONS.md` in this
record's freeze PR, re-verifying next-free ids against main AND every open
design PR at freeze time (the documented DL-264 collision precedent: a
sibling record "also claimed DL-264 and merged first").

## Tasks

- [ ] T0: proto arms 14/15 + request messages + regen
- [ ] T1: `Provider.TransitionIssueState` / `TransitionPullRequestState` +
      `TransitionState` input + fake
- [ ] T2: GitHub PATCH implementations + golden fixtures + livegithub legs
- [ ] T3: Linear `issueUpdate` implementation, workflow-state name/type
      resolution + default rule + fixtures + livegithub legs
- [ ] T4: server arms (validate → write → flatten → memo → result) +
      `forge_state_transitions` migration + store surface + tests
- [ ] T5: STATE actor-resolution seam over the memo (the RIG-3326 contract
      surface) + both-lane wiring + pgtests
- [ ] T6: `forge_transition_issue_state` / `forge_transition_pull_request_state`
      tools
- [ ] T7: live-oracle cross-op sweep
- [ ] T8: ledger rows appended (id re-verify at freeze)

## Ledger impact

Ledger-impact: adds two rows to `docs/designs/DECISIONS.md` (Comms & tools
section, beside DL-241/DL-276):

- **DL-342** — the forge state-transition op: portable `{open, closed}` core +
  per-provider refinements (`close_reason` / `workflow_state`), fail-loud
  in-band `invalid_argument` on refinement/provider mismatch, `ErrUnsupported`
  on the Linear PR half; transitions are NOT F3-deduped and NOT
  DL-055-recorded (mutate-existing-coordinate, comment-arm precedent); amends
  DL-241's tool count by citation (twelve tools, rule unchanged).
- **DL-343** — transition actor attribution via the consumable
  `forge_state_transitions` memo (write-after-success at the chokepoint,
  consume-on-match at the notify lane), never a parsed-text or author-row
  proxy; the contract RIG-3326's STATE suppression arm keys on.

Numbering: the highest row on current main is **DL-337**. Two of my own design
PRs are open ahead of this one and claim the next four ids: the RIG-3299
self-delegate record (#900) declares **DL-340**, and the RIG-3326 suppression
record (#913) declares **DL-338** and **DL-339**. So DL-342/DL-343 are the
next free pair, and both MUST be re-verified as next-free at freeze against
main and every open design PR — enumerating *every* open design PR, not just
the adjacent one (the DL-264 collision precedent; an earlier draft of this
section counted #913 alone and collided with #900's DL-340).

## Open Questions

- **OQ-1 (load-bearing): actor-carrier mechanism — memo-consume (recommended)
  vs synthetic event at the chokepoint.** This record recommends the durable
  memo consumed at the notify lane (§Actor attribution; alternatives weighed
  in §Alternatives considered). Needs Matt because the consumer contract
  crosses two records: the RIG-3326 record's frozen text describes the actor
  as stamped "onto the emitted event" by this op, which reads as the synthetic
  shape, while the memo attributes the provider-echoed event instead —
  equivalent for the suppression outcome, different in mechanism, and only
  Matt can rule which side's text bends (this is a cross-record contract
  question, not an implementation detail this record may decide alone).
- **OQ-2 (load-bearing): the Linear default-state rule.** When
  `workflow_state` is empty: close ⇒ lowest-positioned `completed`-type state;
  open ⇒ lowest-positioned `unstarted`-type (fallback `backlog`). Needs Matt
  because the rule silently chooses a human-visible board column on every
  default close/reopen an agent performs — a product-behavior call, not a
  code-shape call. Alternative: make `workflow_state` REQUIRED on Linear
  (no default at all), trading portability for explicitness.
- **OQ-3 (non-load-bearing, deferred): memo freshness bound.** The
  consume-on-match window (proposed: minutes-scale, exact constant at
  execution) trades a late webhook's missed attribution (fail-open, one
  redundant wake) against a stale memo mis-attributing an unrelated later
  transition (bounded by the state-match predicate already). Any value in the
  minutes range is safe; the executor picks it with a named constant. Deferred
  with rationale — the design is correct under any bound.
