# Design: Forge self-delegate write path (Record A)

Status: Active

## Problem / Intent

Compass agents file Linear issues but cannot take delegation of them — mark a
filed issue as "mine". The forge write path sends neither `assigneeId` nor
`delegateId` (`go/internal/forge/linear.go:160` builds only
`teamId`/`title`/`description` + attribution). This record adds the **outbound
self-delegate write path**: an agent-filed issue can be delegated to the acting
app itself, optionally alongside a human assignee.

**Scope is deliberately the outbound half only.** The inbound leg — a human
delegating an issue to the app, firing a `created` AgentSessionEvent the
responder routes to a Manager (DL-254/255/256/302; the DL-309 live round-trip,
ratified and never built) — is **Record B**, sequenced behind Matt's RIG-3271
gating-posture ruling (a new live-Linear leg on the required gate is the exact
flake class RIG-3271 is escalating). Record A adds no inbound production code and
changes no responder behavior; T4 asserts the merged create path and the merged
routing resolver (`ResolveResponder` + the store-backed `OwnershipIndex`) end to
end, supplying a TEST `ManagerResolver` for the manager-walk seam the RIG-2717
assembly has not yet implemented in production (`routing.go:54-56`; the only
`OwningManager` impl in-tree is the test fake, `routing_test.go:70`). Its touched
files are `forge/provider.go`, `forge/linear.go`, `forge/testdata`,
`forge/livegithub_test.go`, `tools/forge-linear-token`, `golden_test.go`, plus
`go/server/forge.go` (a call site the T2 struct change forces to recompile, not a
behavior change) and a NEW `go/server` routing e2e (T4) that drives the merged
create arm + resolver read-only (only `package server` can construct
`forgeService`). Record B owns the tunnel INGRESS and any responder-side CHANGE;
the two records share no responder production code. The one question that
straddles them — whether an app-set `delegateId` fires a `created` webhook —
is a Record-B question and is NOT on Record A's critical path (see the
self-origin note in Approach and T2); the only thing that once coupled the
credential — a Linear user-credential reversal of DL-324 — is now shown
unnecessary for this record (see Approach).

## Approach

**Wire `delegateId` (the acting app, self) + `assigneeId` (a human) into the
Linear provider's `issueCreate`, Linear-only. The live permission probe that
gated this record is RESOLVED (2026-09-05): self-delegate is allowed and the
assignee slot is independent (§Resolved decisions). Matt ruled (2026-09-05) the
outbound shape ALWAYS sets both — a human assignee AND the app delegate —
never delegate-only: Linear's own UI does not offer delegate-without-assignee,
so relying on that shape would diverge from how the product is actually used.**

### Ground truth (Linear GraphQL schema + dev docs, verified 2026-09-05)

- `IssueCreateInput` carries BOTH `assigneeId: String` ("user to assign the
  issue to" — a human) AND `delegateId: String` ("the identifier of the AGENT
  user to delegate the issue to"), independent optionals; `IssueUpdateInput.delegateId`
  exists too. "Human assignee + agent delegate" is expressible in ONE
  `issueCreate`, and a delegate does NOT require an assignee.
- `Issue.delegate: User` is a schema field distinct from `assignee` — Linear's
  docs: "Agents are not traditional assignees. Assigning an issue to an agent
  delegates the issue to that agent while the human teammate remains the primary
  assignee and owner."
- `createAsUser` is "only available to OAuth applications creating issues in
  `actor=app` mode" — compass already rides it (`linear.go:606` `applyAttribution`).
- `agentSessionCreateOnIssue(input: {issueId})` lets an app PROACTIVELY open its
  own agent session on an issue without being delegated/mentioned — Linear's
  documented "work on my own issue" path (weighed under Alternatives).
- **RESOLVED (probe, 2026-09-05 — see §Resolved decisions):** an `actor=app`
  client-credentials token (scope `read,write,app:assignable`) MAY set
  `delegateId` to its OWN app-user id (self-delegate), and the create leaves the
  assignee slot untouched — no `actor=app` auto-assign (there is no acting human
  in `actor=app` mode). **Still a Record-B question, NOT this record's critical
  path:** whether an app-set `delegateId` FIRES a `created` AgentSessionEvent
  (the self-origin-loop question) — observing that requires a public webhook
  ingress, which only Record B builds (see the self-origin note below and T2).

### The scope question (an assumption the probe TESTS, not a presupposed fact)

The live oracle mints its `LINEAR_FORGE` token with `SCOPES = "read,write"`
(`tools/forge-linear-token/index.ts:38`); the production Linear client requests
`"read,write,app:assignable,app:mentionable"` (`go/internal/linearagent/client.go:30`).
`app:assignable` is documented as "allow the app to be assigned as a delegate on
issues" — which describes the **inbound** capability (the app appearing in
Linear's delegate picker so others may delegate TO it), NOT necessarily the
**outbound** write of setting `delegateId` on an `issueCreate`. Whether the
outbound self-delegate write needs anything beyond `write` was part of what the
probe tested. **Probe outcome (2026-09-05):** the probe ran with an
`app:assignable`-scoped token (Matt enabled `app:assignable` on the testbed
OAuth app for the RIG-3302 human action) and self-delegate SUCCEEDED. Whether
plain `read,write` alone would suffice was left untested — re-running to find
out would only risk a false-negative, and the production Linear client already
requests `app:assignable` (`client.go:30`). So this record ADOPTS
`app:assignable` as the confirmed oracle mint scope, and T0 (the mint-scope
bump) is UNCONDITIONAL.

Note the token-scope hazard on T0's mint change: changing a client-credentials
mint scope carries a documented revocation hazard — "Linear revokes a
client-credentials app's existing tokens when a mint requests a different scope
set" (`client.go:26-29`). Adding `app:assignable` (a superset) to the testbed
app's per-run mint is low blast radius (oracle tokens are per-run and
short-lived; no concurrent run depends on a specific token surviving), but the
change lands as its own step (T0) and is called out.

### Shape of the change

Extend `forge.CreateIssue` (`go/internal/forge/provider.go:175`) with two fields
— `DelegateSelf bool` and `Assignee string` — consumed only by the Linear
provider. Per Matt's always-assign ruling the outbound path sets BOTH slots: the
human owner as `assigneeId` and the acting app as `delegateId`. `Linear.CreateIssue`
resolves its own app-user id via a `viewer { id }` sibling of the existing
`viewer { app }` probe (`linear.go:563` `actorAttribution`; the two fields
fetched in ONE query, not a second wire call) and sets
`input["delegateId"]`/`input["assigneeId"]`. GitHub's provider ignores
`DelegateSelf` (no delegate concept). Degrade like attribution: a probe failure
creates the issue WITHOUT delegate and logs a warn — never fails the create.

**Assignee source (T2 dependency).** `assigneeId` is a Linear user UUID, but
Compass records an owner only as its internal account id (`OwnerUserID`, a
Compass `store.AccountID`, not a Linear user) — there is no owner→Linear-user
mapping in the code today (verified: `store.AuthoredArtifact.OwnerUserID` and
`AgentAccount.OwnerUserID` are Compass account ids). Record A wires NO production
caller (T2 ships plumbing only), so it builds NO owner→Linear-user resolver:
`Assignee` is a caller-supplied provider input, empty until the first production
caller (a later record) resolves the owner's Linear identity and passes it — the
provider never defaults or invents it. The only place a concrete assignee UUID is
needed in THIS record is T3's live leg, which sources it from the
`LINEAR_FORGE_ASSIGNEE` testbed secret (§T3), not from any owner mapping.

The delegate/assignee slots are read back only in the live tests' raw reads
(`issue { delegate { id } assignee { id } }`); `forge.Issue` gains NO public
fields and the `provider.go:34-37` boundary comment ("Compass machinery
(id/state-lifecycle/priority/assignee/prs/tracker) is added there, not carried
here") stays intact. Promoting the slots onto `forge.Issue` is a later record's
call, when a server path actually consumes them (decision folded from review M6:
read-only-in-tests, smaller surface, boundary comment unchanged).

No new `Provider` interface method: the interface is "one method per forge
operation the Server drives" (`provider.go:236`), and nothing server-side drives
delegate-after-create. An `issueUpdate`-based delegate path is out of scope for
this record (it belongs to Record B's round-trip trigger).

### Self-origin webhook loop — a Record-B question, stated here for the executor

If an app-set `delegateId` fires a `created` AgentSessionEvent, production
self-delegation on every compass-filed issue would loop into our own responder
(`NewLinearWebhookHandler` → `Dispatcher.Enqueue` → `ResolveResponder`; a
compass-authored issue HAS an ownership row (DL-055), so `routing.go:110-115`
walks it to the owning Manager) — a spurious session + Manager prompt per
self-delegated create. **Whether it fires cannot be observed in Record A** (no
live webhook ingress exists here; that is DL-309/Record B). Note the loop is
not live today regardless: production passes a nil `sessionSink`
(`serve.go:1087`), so Linear session events are logged-and-dropped until the
RIG-2717 responder assembly wires a real `Dispatcher` — the deferral below is
forward-looking, guarding the moment that assembly lands. So:

- Record A ships the write path with self-origin suppression **unresolved**, and
  that is an accepted cost: until Record B's probe answers "fires or not", the
  outbound `DelegateSelf` capability should not be enabled on a production code
  path that a live Manager watches. Record A lands the provider plumbing +
  hermetic/live *write-leg* coverage; wiring any server caller to set
  `DelegateSelf: true` in production waits on Record B's answer.
- If Record B's probe shows `created` fires, self-origin suppression is owned by
  the RIG-3326 record
  (`docs/designs/server/compass-forge-self-delegate-suppression/design.md`), NOT
  Record B — Record B owns only the ingress and the fires/does-not-fire answer.
  RIG-3326 specifies the suppression mechanism (a handle-keyed actor==subscriber
  match at the notify-router fan-out), superseding this record's earlier
  recency-window sketch.
- **OQ-4 (deferred, Record B):** does an app-set `delegateId` fire a `created`
  AgentSessionEvent? Unobservable without a public webhook ingress (only Record
  B builds one); owned by Record B. Named here because T1 and §Approach reference
  it.

### Ledger delta (stated here; the CALLER edits DECISIONS.md in the SAME PR)

- **New row DL-340 (delegation write path):** the forge Linear provider sends
  `delegateId` (self) and optional `assigneeId` on `issueCreate`, Linear-only
  consumption on `forge.CreateIssue`, degrade-on-probe-failure. The OQ-1 probe
  (2026-09-05) confirmed self-delegate is ALLOWED, so DL-340 ratifies the
  self-delegate write path (not the `agentSessionCreateOnIssue` fallback). The
  ledger append lands in this same freeze PR.
- **No DL-324 reversal in this record.** DL-324 (app-actor-only, no Linear user
  credential) stays live for the outbound path — the outbound write uses the
  existing `LINEAR_FORGE` app token, no user credential. Any credential question
  belongs to Record B and only if its inbound round-trip cannot be triggered by
  the app token itself (the inbound test surface — `SessionEvent` has no actor
  field, `webhook.go:19-27`; routing keys only on the issue coordinate,
  `routing.go:110-115` — cannot distinguish who delegated, so if the app can
  self-delegate-and-trigger, the app token drives the round-trip with zero new
  standing credentials).

## Alternatives considered

- **`agentSessionCreateOnIssue` instead of self-delegate** for "the agent works
  on the issue it filed": Linear's documented proactive path — the app opens its
  own agent session on an issue with no delegation or mention. Pros: no
  unverified permission (an explicit app-actor affordance), no assignee-slot
  ambiguity. Cons: does NOT populate `Issue.delegate`, so the issue shows no
  delegate in Linear's UI/board filters; creates a session (10s liveness SLA,
  `dispatcher.go:14-19`) when the product intent is "mark this issue as mine".
  On the loop axis: `agentSessionCreateOnIssue` fires `created` BY CONSTRUCTION
  (it creates the session); whether self-delegate fires `created` is UNVERIFIED
  (the Record-B question above). So the loop axis is a wash ONLY IF that question
  comes back "fires"; if it comes back "does not fire", self-delegate strictly
  dominates on this axis and no suppression is ever needed.
  **Not taken:** the OQ-1 probe (2026-09-05) confirmed self-delegate is ALLOWED,
  so `agentSessionCreateOnIssue` was NOT needed as a fallback. It stays recorded
  as the alternative the record would have pivoted to had the probe forbidden
  self-delegate.
- **A new `Provider.UpdateIssue`/`SetDelegate` interface method** instead of
  create-time fields: rejected — the Provider interface is "one method per forge
  operation the Server drives" (`provider.go:236`); no server path drives
  delegate-after-create. Revisit when one does.
- **Reversing DL-324 to re-add a Linear user credential** (the prior draft's
  bundled choice): rejected for this record — the outbound path needs no user
  credential (it uses the app token), and the inbound path (Record B) likely
  needs none either. The reversal is not this record's to make.

## Global Constraints

- Fixture `repo` values reflect the live testbed team key (`LINEAR_FORGE_TEAM`,
  `livegithub_test.go:66` `envTeam`, sourced from the Actions secret at
  `ci.yml:2407`), NOT the retired literal `"SEA"`. The `-update` capture table
  already covers all four Linear fixtures (`linearUpdateSpecs`,
  `livegithub_test.go:1119-1197` — including the `comment_on_issue` spec at
  `:1176`, `Repo: team`), each writing `repo` from `LINEAR_FORGE_TEAM`, so ONE
  regen run normalizes every `repo` field — no fragment-driven selection, no
  manual per-fixture touch. The pre-convention `"SEA"` literals are all six:
  `create_issue.json:5`, `get_issue.json:5` and `:13`, `list_issues.json:5` and
  `:15`, `comment_on_issue.json:5`. (No `sea-ref-gate` enforces bare `"SEA"` —
  `tools/sea-ref-gate` matches only the `\bSEA-\d+\b` issue-ref token — so this
  is a hygiene normalization, not a gate requirement.)
- No planning metadata (issue ids, task labels) inline in code; ledger/record
  references live in doc comments only where the surrounding files already do.
- Never reference superseded tooling/credential names in NEW code.
- Tests: NEVER `time.Sleep`. `createWithBackoff` (`livegithub_test.go:646`) is a
  bounded ONE-SHOT ctx-aware backoff for a single create (its doc,
  `livegithub_test.go:642-643`), NOT a poll-until-condition helper — no
  bounded-poll helper exists in the file. A task needing read-after-write polling
  must WRITE one, with its bound and ctx discipline stated as an explicit
  deliverable.
- Two forge test tiers only (DL-210): hermetic golden-fixture replay
  (untagged) + `//go:build livegithub` live oracle. No third tier.
- Live Linear legs gate on env `LINEAR_FORGE` + `LINEAR_FORGE_TEAM` via
  `requireLinear` (`livegithub_test.go:91-99`); the app token is minted per CI
  run by `tools/forge-linear-token/index.ts`. The mint's scope string is
  load-bearing: a differing mint scope revokes in-flight tokens (`client.go:26-29`).
- **The one mint scope** used by T0/T1/T3 is `"read,write,app:assignable"` (no
  `app:mentionable` — the oracle drives no mention path). T0 owns changing it;
  T1/T3 reference this constant, never re-spell it.
- Live teardown archives every created issue (`archiveLinearIssue`,
  `livegithub_test.go:861`) — new legs reuse it.
- Do not vary the `Provider` interface (`provider.go:236`); delegation fields
  ride the existing `CreateIssue` input struct.

## Plan

Execution order is **T0 → T1 → T2 → T3 → T4** (T0's mint-scope bump is the
unconditional prerequisite the live legs need; it lands first, never mid-run).
The subsections below keep their original planning numbers, not execution order.

### T1 — Live self-delegate permission probe (GATE SATISFIED 2026-09-05; see §Resolved decisions)

The pre-freeze gate is RESOLVED: the manual probe ran against the live testbed
(app user "Compass Live Tests", `da5c6aa4-…`) with an `app:assignable`-scoped
token and confirmed OQ-1 (self-delegate ALLOWED) and OQ-2 (assignee slot
independent) — folded into §Resolved decisions. (OQ-4 — does self-delegate fire
a `created` webhook — remains explicitly NOT on this gate; it needs Record B's
ingress. See the self-origin note in Approach.)

The remaining implementation deliverable codifies the probe as a live
regression:

Interfaces:

- A `//go:build livegithub` `TestLiveLinearSelfDelegateProbe` in
  `go/internal/forge/livegithub_test.go` (gated by `requireLinear`, teardown via
  `archiveLinearIssue`), using the file's existing direct-POST pattern
  (`livegithub_test.go:874`): `viewer { id app }` → `issueCreate` with
  `delegateId: <viewer.id>` (no `assigneeId`) → read back
  `issue { delegate { id } assignee { id } }`, asserting the delegate slot is the
  app user and the assignee slot stays empty; plus an `issueUpdate` leg covering
  the update-input path. This leg deliberately probes the RAW Linear API
  capability (delegate-only), NOT the Compass write shape — Resolved decision 3's
  always-assign rule governs the provider path and is covered by T3; delegate-only
  is pinned here only to keep OQ-1/OQ-2's probe outcome from silently regressing.
  No production code in this task (the mint-scope change is T0).

### T0 — Oracle mint scope (CONFIRMED REQUIRED; the probe path used `app:assignable`)

Interfaces:

- `tools/forge-linear-token/index.ts:38` — `SCOPES` →
  `"read,write,app:assignable"` (the Global-Constraints pinned string), with the
  revocation-hazard callout (`client.go:26-29`) in the change description. Its
  own small step, landed distinctly. Merging it revokes in-flight oracle tokens,
  so it lands ahead of T1/T2/T3 (which need the scope), never mid-run.
- Human action — DONE (RIG-3302, 2026-09-05): Matt enabled `app:assignable` on
  the testbed Linear OAuth app (`LINEAR_FORGE_CLIENT_ID`); the client-credentials
  mint now returns `scope: "app:assignable read write"`.

### T2 — Provider input plumbing: `DelegateSelf` + `Assignee` on `CreateIssue`

Lands after T0 (the `app:assignable` mint bump). The T1 gate confirmed
self-delegate is allowed, so this task builds the ratified write path (no pivot
to the `agentSessionCreateOnIssue` fallback).

Interfaces:

- `go/internal/forge/provider.go` — `type CreateIssue struct` gains
  `DelegateSelf bool` (Linear-only; GitHub ignores) and `Assignee string`
  (Linear user UUID for `assigneeId`; empty = unset; GitHub out of scope,
  documented on the field). `forge.Issue` and the `provider.go:34-37` boundary
  comment are UNCHANGED (M6 decision: slots read only in live tests).
- `go/internal/forge/linear.go`:
  - Extend the `actorAttribution` probe (`linear.go:563`) to fetch
    `viewer { id app }` in ONE query; cache the app-user id under the same
    mutex/probe-done discipline. New accessor `appUserID(ctx) (string, bool)`
    returns `ok=false` whenever the probe's `actorCapable` is false (a plain
    user/API-key principal — `viewer.id` is still a valid id but is NOT an app
    user, so delegating to it would delegate to a human or error), not merely
    when the id is empty.
  - `CreateIssue` (`linear.go:155`): set `input["delegateId"]` ONLY when
    `in.DelegateSelf` AND `appUserID` returns `ok=true` (clean probe AND
    `actorCapable`); set `input["assigneeId"]` when `in.Assignee != ""`. Degrade
    like attribution (probe failure OR non-app actor → create without delegate,
    log warn, never fail).
  - `issueFieldsFragment` (`linear.go:678`) gains `delegate { id displayName }`
    (+ `assignee { id displayName }`) so the live tests can read back the slot.
- **Fixture regen:** the new `issueFieldsFragment` text (delegate/assignee)
  lands in the three fragment-carrying Linear fixtures
  (`testdata/linear/create_issue.json:17`, `get_issue.json:10`,
  `list_issues.json:12`); the `-update` capture lane regenerates all four Linear
  fixtures in ONE pass (`linearUpdateSpecs`, `livegithub_test.go:1119-1197`),
  each writing `repo` from `LINEAR_FORGE_TEAM` — so `comment_on_issue.json`
  (no fragment) is normalized by the same run, not a separate manual touch.
  Budget T2 for the full four-fixture Linear regen.
- **Self-origin notification suppression is NOT in this record** — it is its own
  design record (RIG-3326: never notify an agent of its own forge action, keep
  the CI consequence; identity match keyed on handles). T2 does not wire any
  production server caller to set `DelegateSelf: true`; it lands the plumbing +
  test coverage only.
- Unit tests (`linear_test.go`): the degrade path (probe error → no `delegateId`,
  create still succeeds); the emitted-mutation-body assertion (delegate id
  present after a scripted `viewer{id,app:true}` probe response); and the
  non-app-actor case (a scripted `viewer{id:<uuid>, app:false}` response emits
  NO `delegateId` — the M3 delegate-to-a-human guard).

### T3 — Live delegation write legs (livegithub oracle + golden capture)

Interfaces:

- `go/internal/forge/livegithub_test.go`:
  - `TestLiveLinearCreateIssueSelfDelegate` — create with `DelegateSelf: true`
    AND a human `Assignee` (the always-assign shape, Resolved decision 3), read
    back, assert BOTH slots: delegate = the app user, assignee = the passed
    human. The human assignee UUID comes from a NEW env var
    `LINEAR_FORGE_ASSIGNEE` (a testbed human user UUID, an Actions secret
    alongside `LINEAR_FORGE_TEAM`; `requireLinear`-style skip when unset, plus a
    matching `ci.yml` env addition in the forge-oracle lane) — nothing in the
    provider resolves an owner→Linear-user mapping (§Shape of the change).
    Teardown via `t.Cleanup(archiveLinearIssue)` — every new live leg cleans up
    after itself (the established pattern; no created issue leaks on the TEST
    team).
- Golden capture: extend the untagged replay harness so the hermetic fixture can
  express delegation — `fixtureInput` (`golden_test.go:81-88`) gains
  `DelegateSelf bool` + `Assignee string`, threaded through `invoke`'s Linear
  `create_issue` arm (`golden_test.go:269-271`); the capture reuses `op:
  "create_issue"` with delegate inputs (no new op arm) via the `-update` lane
  (capture table `linearUpdateSpecs`, `livegithub_test.go:1119`; op dispatch
  `invoke`, `golden_test.go:231`; CI `-update` invocation `ci.yml:2415`).
  Without extending both `fixtureInput` and `invoke` the captured fixture would
  replay a NON-delegate create and assert nothing about the new path. Its capture
  leg cleans up via `t.Cleanup(archiveLinearIssue)`. Adds `golden_test.go` to
  this record's touched files.
- (The mint-scope edit lives in T0, not here.)

### T4 — End-to-end routing e2e (create → stamp → route to owning Manager)

Matt ruled (2026-09-05) the routing flow is exercised end-to-end in THIS record,
hermetically — the live webhook-FIRES leg (does an app-set `delegateId` actually
emit a `created` AgentSessionEvent, OQ-4) stays Record B behind the DL-309
tunnel and RIG-3271. This is a NEW `//go:build pgtest && unix` file in `package
server` (only `package server` can construct `forgeService` —
`go/server/forge_e2e_pgtest_test.go:23-28`), alongside the sibling e2e tests,
driving the chain deterministically over `forge.FakeProvider`
(`forge_e2e_pgtest_test.go:34-39`): NO live Linear call, NO token, NO third test
tier (it stays in the pgtest tier the sibling files already use):

1. A create through the Server create arm (`server/forge.go` `createIssue`) over
   `FakeProvider` so the owner stamp (`StampOwner`) AND the DL-055 ownership row
   (`s.record` → `RecordAuthoredArtifact`) both land — the same create path
   production uses, NOT a direct `provider.CreateIssue`.
2. Inject a synthetic `created` `SessionEvent` for that issue's coordinate
   through the responder chain (`NewLinearWebhookHandler` → `Dispatcher` →
   `ResolveResponder`, `internal/linearagent`) wired with a TEST `ManagerResolver`
   (the manager-walk interface has no production impl yet — `routing.go:54-56`,
   `NewResolver` :81 has zero non-test callers; T4 supplies its own fake exactly
   as `routing_test.go:70` does), and assert it resolves to the AUTHORING agent's
   OWNING MANAGER + home channel — NOT the supervisor fallback. A companion
   negative: an issue with NO recorded row falls back to the supervisor (guards
   against a resolver that always finds an owner).

**The self-delegate write is NOT exercised here** — T4 asserts routing
correctness, and `ResolveResponder` keys ONLY on the issue coordinate
(`routing.go:103`, `:122-127`), never on the delegate slot. Driving a
self-delegate through the Server arm would require a proto `CreateIssueRequest`
field + arm wiring that T2 deliberately does NOT add (the arm builds
`forge.CreateIssue{Title, Body, Labels}` from proto fields only,
`server/forge.go:388`, `agent_gateway.proto:316-321`), so the delegate write
stays covered by T2's unit tests + T3's live leg; T4 needs only the create +
stamp + route legs, which the delegate slot does not touch.

Because the create runs over `FakeProvider` no Linear issue exists, so there is
nothing to archive — the test needs no `archiveLinearIssue` teardown (that
helper is `package forge`, `//go:build livegithub`, unreachable from `package
server` anyway). It exercises the real create path production uses and the
merged resolver, with a test `ManagerResolver` for the not-yet-assembled
manager-walk seam (production wires a nil `sessionSink` today —
`serve.go:1087`). This asserts the coordinate written at create (`iss.Number`,
team-key repo, config host) matches the coordinate the responder parses from the
webhook identifier ("TEAM-NUMBER") — the alignment routing correctness depends
on.

## Tasks

- [ ] T0: `tools/forge-linear-token/index.ts:38` SCOPES →
      `"read,write,app:assignable"` (confirmed required; testbed app-config human
      action already done, RIG-3302). Lands first, ahead of T1/T2/T3.
- [x] T1a gate: manual self-delegate permission probe RESOLVED (2026-09-05) —
      OQ-1 allowed, OQ-2 slots independent (§Resolved decisions). (OQ-4 is Record B's.)
- [ ] T1b: land `TestLiveLinearSelfDelegateProbe` as a live regression (needs
      T0's `app:assignable` mint scope).
- [ ] T2: `CreateIssue.DelegateSelf`/`Assignee` + `viewer{id app}` probe +
      `delegateId`/`assigneeId` wiring + fragment change + full four-fixture
      Linear regen (repo/SEA normalization) + degrade-path unit tests.
      `forge.Issue` unchanged; no production caller wired; no suppression here.
- [ ] T3: Live write legs (`TestLiveLinearCreateIssueSelfDelegate`, always-assign
      shape asserting BOTH slots) + golden-capture scenario; every new live leg
      cleans up via `t.Cleanup(archiveLinearIssue)`.
- [ ] T4: End-to-end routing e2e (`package server`, `pgtest && unix`, over
      `FakeProvider`) — stamped Server create → inject synthetic `created` event
      through the responder (with a test `ManagerResolver` for the unimplemented
      manager-walk seam) → assert routes to owning Manager (+ negative: no-row →
      supervisor). No self-delegate leg (routing keys on coordinate only).
      Hermetic (no live Linear call, no teardown); the live webhook-fires leg
      (OQ-4) stays Record B.

## Resolved decisions

1. **Self-delegate permission (OQ-1) — ALLOWED** (probe, 2026-09-05, live
   testbed). An `actor=app` client-credentials token scoped
   `read,write,app:assignable` set `delegateId` to its OWN app-user id
   ("Compass Live Tests", `da5c6aa4-9f8f-4a3c-8157-c127d34adb99`) on `issueCreate`
   and the create returned `success: true` with the delegate slot populated
   (PROBE issues TEST-816 delegate-only, TEST-817 assignee+delegate; both
   archived). So the outbound self-delegate write path is ratified (DL-340); the
   `agentSessionCreateOnIssue` fallback is NOT taken.
2. **Assignee slot (OQ-2) — independent of the delegate slot.** Delegate-only
   (no `assigneeId`) left `assignee: null` — no `actor=app` auto-assign (unlike
   the human-actor UI). A human `assigneeId` + app `delegateId` in ONE
   `issueCreate` coexist: TEST-817 came back `assignee: Matt Wilkinson`,
   `delegate: Compass Live Tests`. So "human assignee + app delegate" is a
   supported shape, and the two slots never contend.
3. **Outbound shape (OQ-3) — ALWAYS assign a human AND delegate the app**
   (Matt, 2026-09-05). The write path never uses the delegate-only shape:
   Linear's UI does not offer delegate-without-assignee, so a delegate-only
   create would diverge from how the product is actually used. `assigneeId` (the
   human owner) is set on every self-delegated create, alongside `delegateId`
   (the app). This supersedes the prior draft's "leave `Assignee` empty by
   default" recommendation. The Linear assignee UUID is a caller-supplied input
   (see Shape of the change → Assignee source); the provider never defaults it.
