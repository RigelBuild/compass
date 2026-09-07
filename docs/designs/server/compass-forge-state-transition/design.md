# Design: Forge state-transition write op (RIG-3331)

Status: Active

## Problem / Intent

A Compass agent cannot change the state of a forge artifact. The `forgeService`
write arms are create / comment / review, plus get / list / subscribe — no arm
transitions an issue or PR between open and closed, and no `forge.Provider`
method exists for it. Matt ruled (2026-09-05): "we need state transitions 100%.
if they are missing we need to add immediately." This record designs the op:
an agent sets an issue/PR's state on BOTH GitHub and Linear, through the same
attribution chokepoint every other forge write rides — and the acting
agent's identity is recoverable when the resulting STATE event arrives, the
load-bearing contract the RIG-3326 self-origin suppression record keys its
STATE arm on. Per the OQ-1 ruling that identity travels in a consumable
`forge_state_transitions` memo, not on the event itself.

This record is **frozen**. Both load-bearing Open Questions were ruled by
Matt on 2026-09-07: OQ-1 selects the consumable memo as the actor carrier, and
OQ-2 resolved into a reject-when-ambiguous default rule (see §Open Questions
for both rulings and §The cross-provider state model for the resulting
contract). The DL-342/DL-343 rows land in this push (T8), so every task below
is unconditional.

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

The attribution's *source* is the stamped owner header in the comment body.
`stripBodyToRef` (`go/internal/forge/githubapp_webhook.go`) splits the owner
header off the raw body via `StripOwner`, and the detect path
(`go/internal/ingest/notify_detect.go`) builds
`ref.Agent = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}` from
the parsed header. This is the mechanism a state transition CANNOT reuse — a
close/reopen carries no body to stamp, so there is no header to parse an actor
out of. That absence is why this record must specify a new actor carrier at
all, and it is the load-bearing premise under OQ-1.

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
  NAME, resolved per-team (name → id). Empty means the provider maps the
  portable `state` to a default workflow state (rule below).

**Two name-resolution cases the read side never had to face.** Workflow-state
names are not unique, so naming one is not the same as identifying one:

- **Ambiguous within a team.** Linear does not enforce name uniqueness across
  a team's states, so `workflow_state: "Done"` may match two distinct state
  ids on ONE team. This is an in-band `invalid_argument` naming the duplicate
  — consistent with the fail-loud posture above: the caller asked for
  something specific and the write cannot know which was meant. (Alternative
  considered: lowest-position-wins, which silently picks a board column.)
  Without this ruled here, an implementer would have to invent a rule on the
  user-visible write path.
- **The cache cannot be the `teamIDs` cache.** `resolveTeamID` caches a team
  key → UUID **forever, with no invalidation**, and that is correct precisely
  because team UUIDs are effectively immutable. Workflow states are not:
  humans rename, reorder and delete them from the Linear UI, and the default
  rule below depends on *positions*, which drift too. So the workflow-state
  cache is specified separately — a TTL, plus invalidate-and-retry-once when
  an `issueUpdate` fails against an unknown `stateId`. Copying the
  invalidation-free pattern would leave a renamed or deleted state cached for
  the process lifetime, failing every later transition to it until restart.
  This is also the resolve-then-write staleness recovery path.

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

**Linear default-state resolution (when `workflow_state` is empty), per the
OQ-2 ruling (Matt, 2026-09-07): default only where the target is unambiguous, and
reject otherwise.** `state: closed` targets the team's sole workflow state of
type `completed`; `state: open` targets its sole state of type `unstarted`,
falling back to the sole `backlog` state if the team has no unstarted state.
If the resolved candidate set holds more than one state, the op does NOT
guess: it fails in-band with `invalid_argument` naming every candidate and
requiring an explicit `workflow_state`.

This inverts the read-side mapping (`linearClosedStateTypes` = completed,
canceled ⇒ closed; everything else ⇒ open) with one deliberate asymmetry:
default-close resolves against `completed`, never `canceled` — an agent
closing its issue means "done", and "canceled" is reachable explicitly via
`workflow_state`.

Two rejected alternatives, and why this rule beats both. A **positional
tie-break** (lowest-positioned candidate) never fails, which is the problem:
it silently picks a human-visible board column on the caller's behalf, and it
would start guessing years later — the first day someone adds a second
`completed` column — with no test watching the change in behaviour. Making
`workflow_state` **REQUIRED** removes the guess but forces every caller to
fetch the team's workflow states before it can close a Linear issue,
dissolving the portable `{open, closed}` core into a provider-aware callsite
and splitting GitHub/Linear ergonomics. Reject-when-ambiguous keeps the
portable core for the overwhelmingly common single-candidate board while
making a silent column-pick structurally impossible.

Measured on the Rigel team at freeze (2026-09-07): eight workflow states, of which
exactly one is `completed` ("Done"), one `unstarted` ("Todo") and one
`backlog` ("Backlog") — so the default path is unambiguous today and the
rejection arm is a guard against future drift, not a routine outcome. That
measurement is also what retired OQ-2's original framing: the "silent column
choice" it asked Matt to rule on had, on the one board this ships against, no
choice to make.

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
code read above, both create-only mechanisms are inapplicable — but the
transition arm reaches that same conclusion by a **different route than the
comment arm**, and the distinction is load-bearing enough to state before the
bullets: the comment arm is excluded because it has no coordinate at all,
while the transition arm is excluded because the coordinate it targets is an
authorship row it must leave alone. Same outcome, different reason; citing the
comment arm as a bare precedent invites an implementer to reuse its reasoning
where it does not hold:

- **No F3 dedup.** F3 is "create-only per the frozen ruling" (the `record` doc
  comment quoted above), and its memo returns *the recorded coordinate* — a
  transition creates no coordinate to record or return. `client_request_id` is
  already documented as "Ignored on non-create arms" on `ForgeCallRequest`;
  the transition arms inherit that. The idempotency story is the operation's
  own semantics: closing a closed issue is a no-op PATCH on GitHub and a
  same-state `issueUpdate` on Linear — a retried transition converges on the
  target state rather than duplicating anything, which is exactly the hazard
  class F3 exists to prevent on creates and which transitions do not have.
- **No DL-055 row — and this is NOT the comment arm's reason.** The comment
  arm is excluded because a comment is *unrepresentable* in the index: "the
  store index has no comment kind", so it "never reach[es] here". A transition
  is the opposite case. Its coordinate IS representable — `kind` is
  `CHECK IN (1, 2)` = issue|pull_request, exactly what a transition targets —
  and when the acting agent created the artifact a row ALREADY EXISTS there.
  So the transition arm is excluded not for want of a coordinate but because
  that row is a **write-once authorship fact this operation must not touch**.
  Concretely, `RecordAuthoredArtifact` (`go/internal/store/db/forge_authored.sql.go`)
  is an upsert whose `DO UPDATE` sets `session_id`, `client_request_id` and
  `created_at_unix_ms`. Authorship itself survives (the `DO UPDATE`
  deliberately omits `agent_account_id` and `owner_user_id`), but
  `client_request_id` does not — and that column backs the F3 memo through the
  unique partial index `forge_authored_artifacts_request_memo_idx`. **Routing
  a transition through `record` would therefore silently destroy the original
  create's idempotency memo.** Keying suppression off that row is separately
  the author-row-proxy failure RIG-3326 rejects.
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
coordinate in a new store table (`forge_state_transitions`): **`tenant_id`**,
provider, host, repo, kind, number, the applied portable state, the acting
agent's account id, and a written-at timestamp — upserted, latest transition
wins.

**`tenant_id` is not optional, and the guard cannot catch its absence.** The
memo maps a forge coordinate to an acting agent, so it is at least as
tenant-sensitive as its sibling `forge_authored_artifacts`, whose
tenant-in-key design is defended by `TestForgeAuthoredTwoTenantsSameCoordinate`
— two tenants legitimately hold the SAME forge coordinate. Without
`tenant_id`, one tenant's memo could attribute another's STATE event at an
identical coordinate. The RLS catalog test is self-auditing over tables that
*carry* a `tenant_id` column, so a table added without one is invisible to it:
the suite stays green and the leak ships. The column therefore comes with
three explicit obligations in T4 (the `current_setting('compass.tenant_id',
TRUE)` default, the RLS DO-loop array entry, and the `tenantOwned` floor-list
entry), not with a reliance on the pgtest.

When the STATE event for that transition echoes back through the provider (the
GitHub webhook `closed`/`reopened` arm; the Linear data-change
`updatedFrom.stateId` arm) and reaches the router, the router resolves the
event's actor through a package-local seam (the `NotifyStore` / `ChecksRoller`
shape — the `ingest` package follows the **no-store rule**, so the store
enters through a go/server-adapted interface): a point read at the coordinate
that matches the memo's applied state against the event's state within a
freshness bound, and CONSUMES the memo in the same statement (an
`UPDATE … RETURNING`-style one-shot, so one memo attributes at most one
event). A miss — no memo, stale memo, state mismatch — resolves no actor,
which is the correct answer for every human/external transition and the safe
answer for every race (fail-open: an unattributed self-transition costs one
redundant wake, never a lost cross-agent signal).

**Two races decide WHICH event a memo attributes, and both degrade fail-open.**
(1) A single transition can emit a STATE event from both the webhook arm and
the reconcile sweep, since `DetectChanges`
(`go/internal/ingest/notify_detect.go`) emits STATE whenever the fetched state
differs from the prior snapshot; whichever arrives first consumes the memo and
the other is delivered unattributed. (2) `Route`
(`go/internal/ingest/notify_router.go`) upserts the artifact cursor before
resolving subscribers and dispatching, so a route that errors after the upsert
advances the cursor without consuming the memo, which then lingers until it
goes stale. Both cost one redundant wake — the documented safe outcome — so
"one memo attributes at most one event" holds, but which event it attributes
is racy. This is also the real justification for OQ-3's freshness bound.

**Tenant context of the memo read.** The resolve runs under the notify lane's
resolved tenant. `store.WithTenant` has no non-test callers today, so a
webhook/notify-lane store call falls through `resolveTenant` to the bootstrap
tenant — correct and documented on the current single-tenant path. This record
adds the first identity-attribution consumer on that lane, which promotes that
simplification into an attribution-correctness dependency; stated here so
whoever wires a per-tenant ingress later sees the assumption.

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

### T1 — Provider interface + fake + all four implementors

Add `TransitionState` and the two methods to `Provider`
(`go/internal/forge/provider.go`) per §The provider methods; extend `fake.go`
with scriptable implementations (result + error injection, mirroring the
existing per-method fake shape).

**This slice MUST also add the methods to `GitHub` and `Linear`, or it does
not compile.** Four compile-time satisfaction assertions live in the same
package — `var _ Provider = (*FakeProvider)(nil)` in both `fake.go` and
`fake_test.go`, `var _ Provider = (*GitHub)(nil)` in `github.go`, and
`var _ Provider = (*Linear)(nil)` in `linear.go` — so widening the interface
while extending only the fake breaks the GitHub and Linear assertions
immediately. T2 and T3 are exactly what would repair that, and they come
after. So T1 lands the interface together with the Linear PR half as its
specified one-liner (`return PullRequest{}, ErrUnsupported`) and compiling
GitHub/Linear bodies that T2/T3 then fill in with real request construction,
fixtures and error mapping. **No task in this plan may merge red by
construction.**

Interfaces: produces
`TransitionIssueState(ctx, repo string, number uint64, in TransitionState) (Issue, error)`
and
`TransitionPullRequestState(ctx, repo string, number uint64, in TransitionState) (PullRequest, error)`
on `forge.Provider`, consumed by T2/T3/T4.

Tests: fake round-trip in the existing `fake_test.go` style; the package
compiles with all four assertions intact.

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

**Unblocked (OQ-2 ruled 2026-09-07).** This task implements the
reject-when-ambiguous rule: default to the sole candidate of the target type,
and fail with `invalid_argument` naming the candidates when more than one
exists. The close-by-default and reopen-by-default fixtures are correct as
listed, and a multi-candidate fixture is added below to cover the rejection
arm — the guard is the part with no board to exercise it today, so it must be
fixture-driven.

Workflow-state resolution (per-team name→id + type, in its own TTL cache with
invalidate-and-retry-once — NOT the invalidation-free `teamIDs` cache; see
§The cross-provider state model), the ambiguous-name rejection, the
default-mapping rule from §The cross-provider state model, the consistency
check (named state's type must agree with the portable target), the
`issueUpdate` mutation, and `ErrUnsupported` on the PR method.

Interfaces: consumes T1; produces fixtures for close-by-default /
close-by-name / reopen-by-default / unknown-name (`invalid_argument`) /
duplicate-name-within-team (`invalid_argument`) / type-contradiction
(`invalid_argument`) / **two-`completed`-states-with-no-`workflow_state`
(`invalid_argument` naming both candidates — the OQ-2 rejection arm; no
current team reproduces it, so the fixture is the only coverage)**.

Tests: golden replay + `livegithub` Linear legs (gated on the existing
`LINEAR_FORGE` app-actor token per DL-324).

### T4 — Server arms

**Fully in scope (OQ-1 ruled 2026-09-07: the memo).** The two server arms,
their validation screens and the result flattening stand as written, and the
memo half below is now equally in scope rather than contingent.
The memo half (the `forge_state_transitions` table, `RecordStateTransition`,
`ConsumeStateTransition`) exists ONLY under the memo mechanism; if OQ-1 rules
the synthetic-event way, that half is discarded and the actor rides the
emitted event instead. Land the arms first; hold the memo half for the ruling.

The two `forgeService` arms per §The server arm: `resolveTarget`, then arm
validation (state domain; refinement/provider screen; PR-refinement screen),
author-client dispatch, `mapForgeError` flattening, memo write on success,
updated canonical artifact on the result arm (through the existing
`translateIssue` / `translatePR` helpers). Store side: the
`forge_state_transitions` table — which lands **in `0001_init.sql`**, not a
new numbered file: that directory holds exactly one migration by a Matt
ruling that collapsed the original chain into it, and "the same reasoning
folds each later migration in as it accretes". The same edit adds the table's
RLS DO-loop array entry; `tenant_id` and the `tenantOwned` floor-list entry
are obligations of this task per §Actor attribution. Then an upsert write + a
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

**In scope: OQ-1 ruled the memo (2026-09-07).** This task is the memo's read
side and the surface RIG-3326's STATE arm consumes. Its landing is what lets
that record's interim-open STATE arm close, so RIG-3326 is the downstream
consumer of this task specifically.

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

**Done in this push.** The DL-342/DL-343 rows are appended to
`docs/designs/DECISIONS.md` with the ruled wording and the real ruling date
(2026-09-07), which is why they were withheld from the first push: DL-343's
substance *was* what OQ-1 asked Matt to rule, and DL-342's default clause was
downstream of OQ-2. The sibling records that ship rows with the
record (PRs #900 and #932) stamp `Active (Matt, <date>)` because their
content was already ruled;
stamping that here before the rulings would have attributed decisions he had
not made. This satisfies `skill://design`'s same-PR ledger flip, because the
freeze push IS this PR.

Ids re-verified next-free at freeze, against main *and* every open design PR —
not just the adjacent lane's, per the DL-264 collision precedent (a sibling
record "also claimed DL-264 and merged first"). Main's highest is DL-337
(304 rows); #913 holds DL-338/339, #900 holds DL-340, #932 holds DL-341; a
sweep of every open non-queue PR found no other claimant on 342/343. Note
that this check decays as main advances — it was re-run at freeze precisely
because main had moved between the first push and this one.

## Tasks

Both load-bearing Open Questions were ruled at freeze (2026-09-07), so all nine
slices below are unconditional. T3 implements OQ-2's reject-when-ambiguous
rule; T4's memo half and T5 exist because OQ-1 selected the memo.

- [ ] T0: proto arms 14/15 + request messages + regen
- [ ] T1: `Provider.TransitionIssueState` / `TransitionPullRequestState` +
      `TransitionState` input + fake + GitHub/Linear bodies (the interface
      widening and all four implementors land together or the package is red)
- [ ] T2: GitHub PATCH implementations + golden fixtures + livegithub legs
- [ ] T3: Linear `issueUpdate` implementation, workflow-state name/type
      resolution + own TTL cache + name-ambiguity rejection + OQ-2's
      default-or-reject rule (incl. the multi-candidate rejection fixture)
      + fixtures + livegithub legs
- [ ] T4: server arms (validate → write → flatten → result) — unconditional;
      plus the memo half: `forge_state_transitions` in
      `0001_init.sql` with `tenant_id` + RLS array + `tenantOwned` floor
      entry, store surface, tests
- [ ] T5: STATE actor-resolution seam over the memo (the RIG-3326 contract
      surface — landing it closes that record's interim-open STATE arm)
      + both-lane wiring + pgtests
- [ ] T6: `forge_transition_issue_state` / `forge_transition_pull_request_state`
      tools
- [ ] T7: live-oracle cross-op sweep
- [x] T8: DL-342/DL-343 appended to `docs/designs/DECISIONS.md` with the
      ruled wording and the real ruling date, in this PR's freeze push;
      next-free ids re-verified against main and every open design PR

## Ledger impact

Ledger-impact: APPENDS two rows to `docs/designs/DECISIONS.md` (Comms &
tools section, beside DL-241/DL-276) in this push — see T8 for why they were
withheld from the first push:

- **DL-342** — the forge state-transition op: portable `{open, closed}` core +
  per-provider refinements (`close_reason` / `workflow_state`), fail-loud
  in-band `invalid_argument` on refinement/provider mismatch, on an ambiguous
  Linear state name, and on a default resolution with more than one candidate
  state (default only where the target is unambiguous — never a positional
  guess), `ErrUnsupported` on the Linear PR half;
  transitions are NOT F3-deduped and NOT DL-055-recorded — not for the comment
  arm's reason (no coordinate) but because the coordinate's row is a
  write-once authorship fact whose `client_request_id` is the create's F3
  memo; amends DL-241's tool count by citation (twelve tools, rule unchanged).
- **DL-343** — transition actor attribution via the consumable
  `forge_state_transitions` memo (tenant-scoped; write-after-success at the
  chokepoint, consume-on-match at the notify lane, fail-open on a miss), never
  a parsed-text or author-row proxy; the contract RIG-3326's STATE suppression
  arm keys on.

Numbering: the highest row on current main is **DL-337**, and four open design
PRs claim the ids between: **#913** (RIG-3326 suppression) declares DL-338 and
DL-339, **#900** (RIG-3299 self-delegate) declares DL-340, and **#932**
(visual-regression gate) declares DL-341. So DL-342/DL-343 are the next free
pair. Both MUST be re-verified as next-free at freeze against main and every
open design PR — *every* one, not just the adjacent lane's. An earlier draft
of this section enumerated PR #913 alone, proposed DL-340, and collided with
the row PR #900 already claims (the DL-264 collision precedent, reproduced
in draft).

## Open Questions

- **OQ-1 (load-bearing) — RESOLVED (Matt, 2026-09-07): the consumable memo.**
  The actor carrier is the durable `forge_state_transitions` memo, written
  after a successful transition at the chokepoint and consumed on match at the
  notify lane (§Actor attribution). The synthetic-event alternative is
  rejected. Because the RIG-3326 record's frozen text describes the actor as
  stamped "onto the emitted event" — which reads as the synthetic shape — this
  ruling makes **RIG-3326 the side whose text bends**: its STATE arm resolves
  the actor through a memo lookup at the actor-resolution seam rather than off
  the event body. The suppression outcome is identical either way; only the
  mechanism differs. Consequence for the plan: T5 has a subject, T4's memo
  half is in scope, and DL-343 carries this mechanism.
- **OQ-2 (load-bearing) — RESOLVED (Matt, 2026-09-07): default when
  unambiguous, reject when ambiguous.** With `workflow_state` empty, resolve
  the sole candidate of the target type and use it; on two or more candidates
  fail with `invalid_argument` naming them and requiring an explicit
  `workflow_state` (§The cross-provider state model). Both originally-offered
  options were rejected — the positional tie-break because it guesses
  silently, a mandatory `workflow_state` because it forces a provider-aware
  fetch into every callsite. **This question was also ill-posed as filed**:
  it was surfaced as a product-behavior call about "silently choosing a
  human-visible board column", a premise never measured against the target
  board. Measured at freeze, the Rigel team has exactly one `completed`, one
  `unstarted` and one `backlog` state, so there was no column choice to make;
  the ruling therefore converts OQ-2 from a product call into a fail-loud
  code-shape guard against future drift.
- **OQ-3 (non-load-bearing, deferred): memo freshness bound.** The
  consume-on-match window (proposed: minutes-scale, exact constant at
  execution) trades a late webhook's missed attribution (fail-open, one
  redundant wake) against a stale memo mis-attributing an unrelated later
  transition (bounded by the state-match predicate already). Any value in the
  minutes range is safe; the executor picks it with a named constant. Deferred
  with rationale — the design is correct under any bound.
