# Design: Forge scope enforcement — API writes + git operations (A8, Beta tier)

Status: Draft
Owner lane: compass-server (the A5 git-operation leg crosses into
compass-runner at the credential-provision seam — flagged per task). Refs:
RIG-2679 (this record), RIG-2672 (multi-forge widened the blast radius),
RIG-2682 (account model — this record is deliberately independent of its
outcome), RIG-2732 / PR #634 (GitHub App as THE credential — the A5 leg
composes with it).

## Problem / Intent

The forge-write chokepoint ships no server-side scope rejection. The trust
model at the seam says so explicitly:

> Per Resolved decision 2 (MVP, single-trust-domain) the caller is recorded
> for attribution but NO scope rejection ships (A8).
> — the trust-model header comment in `go/server/forge.go`

The frozen forge-write-path record pinned the same posture:

> Authz posture (A8): inherited from the board path — "MVP scope ships no
> scope rejection (single-trust-domain, Resolved decision 2)"; no per-op
> scope check in v1.
> — the frozen forge-write-path record, §T4
> (`docs/designs/server/compass-forge-write-path/design.md`). The board-path
> sentence it quotes is the `BoardCaller` interface comment in
> `go/internal/runnerhub/relay_board.go`.

Meanwhile the credential key deliberately excludes `repo`:

> forgeCoordinate is the registry key: the wire forge enum + host. A repo does
> NOT enter the key — one credential pair serves every repo on a coordinate
> (DL-091 multi-forge disambiguation is provider+host).
> — the `forgeCoordinate` type comment in `go/server/forge.go`

So one shared credential pair serves **every** repo the token can reach, and
for Linear `repo` is a **team key** — "`repo` is the Linear TEAM KEY (e.g.
"SEA"), not owner/name" (the package comment on `go/internal/forge/linear.go`)
— so the RIG-2672 multi-forge coordinate doubled the blast radius.
`buildForgeWriteService` in `go/server/serve.go` registers a Linear coordinate
beside GitHub whenever its `linearTokens` argument is non-nil: the gate is
`if linearTokens != nil`, and that token source is the OAuth
client-credentials `*linearagent.TokenSource` `buildLinearTokenSource` builds
from the declared `LINEAR_FORGE_CLIENT_ID` / `LINEAR_FORGE_CLIENT_SECRET`
secrets — nil when Linear is unconfigured. So a hallucinated or
prompt-injected `repo` string in a `ForgeCallRequest` writes into **any GitHub
repo and any Linear team the shared credential reaches**, attributed but never
rejected.

Matt ruled server-side scope enforcement a **requirement of the Beta tier**,
regardless of the RIG-2682 account-model outcome. Two gaps between that ruling
and what this record ships are disclosed rather than papered over: §A4's
`EnforceScopes` flag is drafted default-FALSE, so a Beta deployment that never
sets it enforces nothing (**OQ-1**), and §A5's PAT-only path leaves the git leg
unenforced (**OQ-6(ii)**). Closing either gap is Matt's call at those forks —
this record does not claim the ruling is already satisfied unconditionally.
The Dogfood tier defers enforcement entirely (single trust domain — one
operator owns every agent and every credential). This record designs the Beta
gate and its Dogfood off switch. Scope of the record: the **forge-API write
chokepoint** (A1-A4) plus the **git clone/push/pull
surface** (A5) — folding in Matt's 2026-08-26 ruling that scope enforcement
"needs to scope the repos the agent can clone/push/pull too". The
forge-API leg is server-authz work only: the TS tool leg already sends `repo`
and is not reworked. The git-op leg scopes the **credential** the agent's
container is provisioned with, not the git calls themselves — there is no
server-side chokepoint on the git path to gate (the Runner deliberately never
clones for the agent; it self-clones post-launch,
`go/internal/runtime/agent_test.go`'s
`TestLaunchOrdersStagesEgressBeforeCheckoutDir`). Git-op scoping is
**GitHub-only**: Linear has no git surface at all — its `repo` is the team
key (the package comment on `go/internal/forge/linear.go`) and there is
nothing to clone, push, or pull.

## Global Constraints

- Go, `go/` module; the chokepoint is `package server` (`go/server/forge.go`).
- Rejection is **in-band**, never a Connect error: a tool-level refusal rides
  the `ForgeCallResult_Error` arm the agent renders — "ONLY a malformed
  request (an unset oneof arm) or a missing caller resolution is a Connect
  error" (the in-band-vs-Connect split in the `go/server/forge.go` header
  comment). The helpers exist: `forgeErr(code connect.Code, msg string)` and
  `forgeErrorResult(fe)`, both in `go/server/forge.go`.
- The not-found/forbidden **merge** is house style: an unauthorized target is
  indistinguishable from a nonexistent one, "so a probe enumerates nothing"
  (the `requireChannelMember` doc comment in
  `go/internal/store/authz.go`); the forge error mapper already flattens
  provider 403 ≡ 404 to a byte-identical `not_found` (`mapForgeError` in
  `go/server/forge.go` — its `case 403, 404:` arm and the doc comment's
  "byte-identical not_found (the #995 flattening — the message is fixed,
  never the forge's, so 403 and 404 are indistinguishable)").
- Store access from the chokepoint goes through the **narrow `forgeStore`
  interface** (`go/server/forge.go`) so the ordering is provable against
  `fakeForgeStore` in the default test lane (`go/server/forge_test.go`), with
  pgtest proving the real backend (DL-174 differential-oracle pyramid).
- Migrations: schema changes fold into `0001_init.sql` per that file's own
  stated convention. A genuinely new higher-numbered migration would execute
  (`Store.migrate` applies any version not yet recorded), so a new file is not
  broken in general — but it cannot carry a change that works by EDITING an
  already-applied migration, and this table's RLS enrollment is exactly that:
  an entry in `0001_init.sql`'s `tenant_tables[]` array. Splitting the two
  halves across files is what fails. Text ids, FK
  `ON DELETE RESTRICT`, coordinate columns aligned to the house 0013 issue
  convention (SMALLINT provider CHECK `IN (1,2,3,4)` + `forge_host` in every
  key). The migrations were squashed, so no `0013_*.sql` file exists — the
  shorthand survives in the tree (DL-163's row text, the forge-poll-driver
  record, and the forge-table section comment in
  `go/internal/store/migrations/0001_init.sql`: "Coordinate-aligned to the
  0013 issue convention: SMALLINT provider enum + forge_host in every key").
  The table that DEMONSTRATES the convention is `forge_repo_subscriptions` in
  `0001_init.sql`.
- Ledger: this record proposes its DL row below; the driver assembles the
  final id into `DECISIONS.md` at PR-assembly time. Do not edit `DECISIONS.md`
  from this record.
- Red → green: every task lands its failing test first.
- Cross-lane seam (A5 only): the narrowed-token mint consumes the
  `account_forge_scopes` allowlist (compass-server) but the credential is
  provisioned by the Runner (compass-runner — `Workspace.Credentials` and
  `Workspace.CredentialSetupScript` in `go/internal/runtime/workspace.go`);
  T4/T4.5/T5 name the owner of
  each half explicitly so neither lane assumes the other ships it.

## Approach

One sentence: a per-account **forge-scope allowlist table** consulted by a new
`requireForgeScope` step in every **write** arm of
`ExecuteForgeCallAsAccount`, after coordinate resolution (and, on the create
arms, after the F3 idempotency-memo check — a memo hit writes nothing) and
before any provider call, rejecting an out-of-scope `(provider, host, repo)`
as an in-band `ForgeCallError{code:"not_found"}` — the exact mirror of comms
channel-membership write authz — gated on by a `ForgeConfig` enforcement
flag Beta deployments set (the flag's default direction is OQ-1) and Dogfood
leaves off.

### The mirror pattern (comms channel membership)

Comms authorizes every channel write through one store-side primitive:

> requireChannelMember is the D9 write-authorization primitive: it verifies
> the actor is a member of channelID and returns ErrNotFound if not.
> — the `requireChannelMember` doc comment in `go/internal/store/authz.go`

```go
if err := requireChannelMember(ctx, tx, m.AuthorAccountID, ChannelID(channelID)); err != nil {
    return Message{}, false, err
}
```

— the D9 write-authz gate inside `AppendMessage`
(`go/internal/store/messages.go`). The refusal is `ErrNotFound`
("channel %q", the not-found/forbidden-merge branch inside
`requireChannelMember`), never a distinct forbidden. Forge scope
enforcement is the same shape with the membership row replaced by a scope row
and the tx-querier replaced by the pool (the forge chokepoint holds no store
tx; its writes are single statements).

One more comms precedent this design leans on for the grant model:

> the actor is authorized when it owns the group, when it is an agent whose
> owning user owns the group (an agent acts within its owner's space — Matt's
> ruling) …
> — the `requireGroupCreateAuthz` doc comment in
> `go/internal/store/authz.go`

### A1 — storage: a new `account_forge_scopes` table

Neither existing table fits. `forge_repo_subscriptions` is the **board poll
target set**, deployment-global with no account column (its `CREATE TABLE` in
`go/internal/store/migrations/0001_init.sql`) — reusing it would conflate
"what the board ingests" with "what an account may write", and disabling a
poll target would silently revoke write scope. `agent_forge_subscriptions`
(same migration) is per-**artifact** notification state, not a repo grant.
So: a new table, coordinate-aligned to the 0013 convention and — because it
is an AUTHORIZATION table — enrolled in the repo's row-level tenant isolation
exactly as every sibling forge table is:

```sql
-- RIG-2679 (A8): per-account forge write scope. A row grants account_id the
-- right to write into (forge_provider, forge_host, repo); repo is the Linear
-- team KEY on LINEAR rows. repo = '*' grants the whole coordinate. Grants
-- attach to the OWNING USER account: the chokepoint checks agent-or-owner,
-- so one grant covers a user's whole agent fleet (an agent acts within its
-- owner's space — the requireGroupCreateAuthz precedent in
-- go/internal/store/authz.go); keying on account_id (not user_accounts) keeps
-- a future per-agent narrow additive. GITHUB repo lowercased at the store
-- door (the forge_repo_subscriptions convention, its CREATE TABLE comment in
-- 0001_init.sql). tenant_id rides the key FIRST, mirroring
-- forge_repo_subscriptions' PRIMARY KEY (tenant_id, forge_provider,
-- forge_host, repo): two tenants may hold the same coordinate without
-- collision, and an authz table must never sit outside tenant isolation.
CREATE TABLE account_forge_scopes (
    account_id     TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT NOT NULL,
    repo           TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id      TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, account_id, forge_provider, forge_host, repo)
);
```

**RLS enrollment, and why it lands in `0001_init.sql`.** The
`ENABLE`/`FORCE ROW LEVEL SECURITY` + `tenant_isolation` policy is applied by
a `DO` loop over an explicit `tenant_tables text[]` array in
`0001_init.sql`, and every sibling forge table is enrolled
(`forge_repo_subscriptions`, `agent_forge_subscriptions`,
`forge_artifact_cursors`, `forge_authored_artifacts`).

**The enrollment and the `CREATE TABLE` MUST land in the same executed
migration, and that migration is `0001_init.sql`.** The licensing fact is the
deployment premise, so state it rather than assume it: `0001_init.sql`'s
History note records "Pre-dogfood — zero users, zero deployed databases", the
basis on which Matt ruled (2026-08-07) to collapse the original chain, and it
is explicit that the fold "is a schema RESET, correct ONLY because no deployed
DB exists to migrate." Every database is therefore recreated on schema change
and migrates from empty, so an edit to `0001_init.sql` executes. **T1 must
re-confirm that premise still holds at implementation time** — the moment a
deployed database exists, folding stops working and this instruction is wrong.

What fails either way is SPLITTING the two halves. `Store.migrate` skips any
migration whose version is already recorded (`if applied[m.version] {
continue }` in `go/internal/store/store.go`) and `applyMigration` has no
re-apply path. So creating the table in a separate `000N_*.sql` while
enrolling it by editing `0001_init.sql`'s array is jointly inert on any
database that has already applied v1: a `tenant_id` column with no policy over
it, fail-open, on an authorization table. If the pre-dogfood premise has
lapsed by implementation time, the fix is not to split — it is a new numbered
migration carrying BOTH the `CREATE TABLE` and its own explicit
`ALTER TABLE … ENABLE`/`FORCE ROW LEVEL SECURITY` +
`CREATE POLICY tenant_isolation` statements inline, never an edit to
`0001_init.sql`'s `tenant_tables[]`.

On the current premise this record folds both halves into `0001_init.sql`, as
`model_registry` did — noting that precedent is one for folding a table, not
for the enrollment half, since `model_registry` carries no `tenant_id` and
needs no array entry. T1 carries both halves as explicit deliverables.

There is a partial backstop, and its limits decide what T1 may lean on.
`TestRLSCatalogEnabledAndForced` (`go/internal/store/rls_pgtest_test.go`)
enumerates every `tenant_id`-bearing table from the live catalog rather than
from a hand-maintained list, and fails any that is not `ENABLE`d and `FORCE`d.
Three limits:

- It reads `relrowsecurity`/`relforcerowsecurity` from `pg_class` and does
  **not** assert that a `tenant_isolation` POLICY exists, so a table that is
  `ENABLE`d and `FORCE`d with no policy over it passes the guard while
  denying all access.
- It is `//go:build pgtest`, so it fires only in the Postgres-backed lane.
- **It cannot observe the split-migration failure at all**, in any lane. The
  harness migrates from empty every time (`RequireDSN` in
  `go/internal/pgtest/pgtest.go` "returns a DSN addressing a freshly-created,
  empty schema", which `newTestStore` then opens and migrates), so nothing is
  ever in the applied set and every migration file executes. The split
  arrangement passes green on a fresh schema; the failure needs an
  already-applied v1, which the harness by construction never produces.

What it genuinely catches is a table folded into `0001_init.sql` whose name
was omitted from `tenant_tables[]` — a real and likely mistake, and the one
T1 may rely on it for. It is not a backstop for the arrangement above, which
is why enrollment stays an explicit T1 deliverable with its own pgtest.

The check is one EXISTS over `(agent OR its owner) × (exact repo OR '*')`:

```sql
SELECT EXISTS (
  SELECT 1 FROM account_forge_scopes s
  WHERE s.forge_provider = $2 AND s.forge_host = $3
    AND s.repo IN ($4, '*')
    AND s.account_id IN (
          $1,
          (SELECT owner_user_id FROM agent_accounts WHERE account_id = $1)))
```

`agent_accounts.owner_user_id` is `NOT NULL REFERENCES user_accounts
(account_id)` (the `agent_accounts` `CREATE TABLE` in
`go/internal/store/migrations/0001_init.sql`), and the chokepoint already
resolves the same edge for attribution (`resolveIdentity` in
`go/server/forge.go`).

Grant and check MUST agree on case. The grant door lowercases GITHUB repos
(the `forge_repo_subscriptions` convention: "For GITHUB the repo string is
lowercased at the seed/upsert boundary", its `CREATE TABLE` comment in
`0001_init.sql`) and preserves the Linear team key verbatim (`repo` is the
Linear TEAM KEY, e.g. "SEA" — the package comment on
`go/internal/forge/linear.go` — never case-folded anywhere in the
store). `HasForgeScope` therefore applies the IDENTICAL provider-aware fold
to the incoming query `repo` before the EXISTS — GITHUB lowercased, LINEAR
preserved — so a mixed-case injected `repo` can neither slip past a
lowercased grant (fail-open) nor a correctly-granted caller miss its own
grant (inconsistently fail-closed).

### A2 — population: declarative seed + owner grant, no console clicks

Two paths, both agent/IaC-friendly (rule no-human-clicks):

1. **Boot seed (MVP, required):** `ForgeConfig` grows
   `ScopeGrants []string` of `handle=provider:host/repo` entries (repo `*`
   allowed), reconciled at serve assembly exactly as `SeedRepos` reconciles
   into `forge_repo_subscriptions` — "bootstrap-only insert, ON CONFLICT DO
   NOTHING" (`reconcileForgeSeed` in `go/server/serve.go` over
   `Store.EnsureForgeRepoSubscription` in
   `go/internal/store/forge_cursors.go`, whose doc comment states the
   posture, is the pattern). The
   deployment's scope set lives in config, deployed by merge to main.
2. **Owner grant RPC (same slice, small):** `GrantForgeScope` /
   `RevokeForgeScope` store methods, exposed later on the admin surface; in
   this slice they exist for the seed reconciler, SQL-parity operators, and
   tests (the `Store.SetForgeRepoSubscriptionEnabled` posture in
   `go/internal/store/forge_cursors.go`). Agents never self-grant — a self-declarable
   allowlist is no allowlist; the granting principal is the owning user (or
   deployment config), which is what bounds the injected-`repo` blast radius.

### A3 — enforcement point and rejection shape

`ExecuteForgeCallAsAccount` (`go/server/forge.go`) dispatches ten arms in one
oneof switch. The five **coordinate write** arms (`createIssue`,
`createPullRequest`, `commentOnIssue`, `commentOnPullRequest`,
`submitReview`) each begin with `resolveTarget(call, repo)` — "It is the
first step of every arm" (the `resolveTarget` doc comment in
`go/server/forge.go`) — which validates the repo and resolves the
coordinate. The gate's slot differs between the two arm shapes, because only
the create arms carry the F3 idempotency memo:

- **Create arms** (`createIssue`, `createPullRequest`): `resolveTarget` →
  `dedup` → *(memo hit returns the recorded coordinate, zero provider calls,
  zero scope check)* → `requireForgeScope` → identity/stamp/write. The F3
  memo lookup (`dedup` in `go/server/forge.go`, called from `createIssue` and
  `createPullRequest` immediately after `resolveTarget`)
  returns an already-created artifact "with ZERO provider calls"
  (the `createIssue` doc comment) — it performs no write, so it needs no
  write scope.
  Gating BEFORE dedup would break the F3 retry contract: a create committed
  while enforcement was off (Dogfood), retried after a Dogfood→Beta flip
  whose grants never seeded that repo, would reject even though the artifact
  already exists and the memo hit would have returned it writing nothing.
- **Comment/review arms** (`commentOnIssue`, `commentOnPullRequest`,
  `submitReview`): `resolveTarget` → `requireForgeScope` →
  identity/stamp/write. These arms have no dedup step to order against —
  "the comment/review arms have no coordinate to record, so they never reach
  here (F3 is create-only per the frozen ruling)"
  (the `record` doc comment in `go/server/forge.go`) — so the gate sits
  immediately after
  `resolveTarget`.

Either way the gate runs before identity resolution, stamping, or any
provider touch:

```go
// requireForgeScope is the RIG-2679 (A8) write gate: the caller (or its
// owning user) must hold an account_forge_scopes row for the resolved
// coordinate+repo. Out of scope renders as the byte-fixed in-band not_found
// (the authz.go not-found/forbidden merge; byte-identical to the
// provider-403/404 flatten text mapForgeError emits), so a probe enumerates
// nothing. Create arms call
// it AFTER the F3 dedup memo check (a memo hit writes nothing, needs no
// scope); comment/review arms directly after resolveTarget. A nil check on
// s.enforceScopes is the Dogfood defer.
func (s *forgeService) requireForgeScope(ctx context.Context, caller store.AccountID, rf resolvedForge, repo string) *compassv1internal.ForgeCallError
```

- **In scope / enforcement off** → nil, arm proceeds unchanged.
- **Out of scope** →
  `forgeErr(connect.CodeNotFound, "forge: artifact not found")` —
  **byte-identical**, as a requirement not a preference, to the text the
  provider 403 ≡ 404 flatten already emits (the `case 403, 404:` arm of
  `mapForgeError` in `go/server/forge.go`, whose doc comment fixes the
  message: "the message is fixed, never the forge's"). A prompt-injected
  probe gets
  the SAME bytes for out-of-scope, nonexistent, and forbidden, so message
  text is no oracle to distinguish them. (This resolves the draft's former
  rejection-text open question in-design. The unconfigured-coordinate
  refusal keeps its distinct text — "forge: no provider configured for the
  requested coordinate", in `resolveTarget`: it varies only with
  deployment config, never with the probed repo, so it leaks nothing about
  targets.)
- **Store fault** → `storeForgeError(err)` (`go/server/forge.go`),
  like every other store touch on the path — fail closed (an error is not a
  pass).

Read arms (`getIssue`, `getPullRequest`, `listIssues`, all in
`go/server/forge.go`) are NOT gated in this
slice: none of the three takes a `caller` parameter today — the dispatch
calls them as `s.getIssue(ctx, call, c.GetIssue)` — Matt's ruling targets
**writes**, and the read
surface leaks only content the shared read credential already exposes to every
agent. Extending the gate to reads is OQ-4.

#### The subscribe arms are write arms, not unimplemented

`Subscribe`/`Unsubscribe` **are implemented on `main`** and both are store
writes that take a caller: `subscribeForge` resolves the target and writes via
`s.store.EnsureAgentForgeSubscription(...)`, and `unsubscribeForge` writes via
`s.store.DeleteAgentForgeSubscription(...)` (both in `go/server/forge.go`,
both `func (s *forgeService) …(ctx context.Context, caller store.AccountID, …)`).
An earlier draft of this record called them unimplemented; that was wrong, and
the error was load-bearing twice over — it would have let the write-arm
exhaustiveness test below classify two real write arms as `unimplemented`
(leaving them permanently ungated, the exact hole the test exists to close),
and it would have made the signature cross-check unsatisfiable on day one.

**Classification: both go in the WRITE set.** They write per-account
subscription rows keyed by a repo coordinate — `EnsureAgentForgeSubscription`
takes `Provider`/`Host`/`Repo` and lands a row in `agent_forge_subscriptions`
— so a caller who may not write into a coordinate should not be able to
attach fleet-visible notification state to artifacts on it either. Their
**gate shape** differs, and the classification map records the shape per arm
rather than hiding the difference:

- `subscribeForge` — **coordinate-gated**, the comment/review shape exactly:
  it already calls `resolveTarget(call, req.GetRepo())` first, so
  `requireForgeScope(ctx, caller, rf, req.GetRepo())` goes immediately after,
  before `subscribeToStoreKind` and the store write.
- `unsubscribeForge` — **caller-scoped-by-id**, and there is nothing for
  `requireForgeScope` to check: the arm carries a `subscription_id` and no
  `(provider, host, repo)` at all (its doc comment: "Unsubscribe is by id, so
  it needs no coordinate resolution"), and the store already scopes the
  delete to the calling agent — "an unknown id, or one owned by another
  agent, is an in-band not_found". Resolving a coordinate solely to gate a
  delete of the caller's OWN row would add a lookup and no authorization.
  So this arm's declared gate is the store's caller predicate, asserted by
  test, not `requireForgeScope`.

This is a deliberate deviation from a flat three-way (write/read/
unimplemented) map: the map is `arm → {class, gate}` where `class ∈
{write, read}` and `gate ∈ {coordinate, caller-scoped}`. A third top-level
class would have hidden `unsubscribeForge`'s real property — it IS a write
and it IS authorized, just not by coordinate — and an `unimplemented` bucket
is exactly the bucket the stale claim above parked two live write arms in.
There is no `unimplemented` set; every arm on `main` is implemented.

Per-arm hand wiring is still how a
FUTURE write arm ships ungated: the next arm lands and nobody remembers the
gate. The slice therefore adds a **write-arm exhaustiveness test** (default
lane, beside the per-arm cases): it walks the `ForgeCallRequest` `call`
oneof's field descriptors — the same ten arms the dispatch switches over
— against the explicit in-test classification
map above. An arm missing from the map fails the
test, so a NEW arm cannot land unclassified; and every write-classified,
coordinate-gated arm is driven with enforcement-on + zero grants, asserting
the byte-fixed `not_found` with zero provider-fake calls, so an UNGATED write
arm cannot land green. The caller-scoped arm (`unsubscribeForge`) is driven
instead against another agent's subscription id, asserting the store's
not_found. A fused resolve-and-gate helper was considered and rejected for
this job — see Alternatives.

The descriptor walk closes the *unclassified*-arm gap, not the
*mis*-classified one: a future genuinely-write arm added AND deliberately
entered in the `read` set slips the driven-enforcement leg. T2
hardens this structurally rather than by convention, with a **signature
cross-check stated as a biconditional over the caller parameter** — the one
form that actually holds on `main`:

> An arm is classified `read` **if and only if** its handler's signature
> takes no `caller store.AccountID` parameter; every `write`-classified arm's
> handler takes one.

On `main` that is exactly satisfied: the three read handlers (`getIssue`,
`getPullRequest`, `listIssues`) take `(ctx, call, req)`, and all seven write
handlers — the five coordinate writes plus `subscribeForge` and
`unsubscribeForge` — take `caller store.AccountID`. The earlier phrasing
("the read/unimplemented sets contain only handlers whose signature takes no
caller") could not pass as written, because it parked the two caller-taking
subscribe handlers in a non-write set.

Both directions are load-bearing, and the second is the one the earlier draft
got backwards. Asserting only "no `read` arm takes a caller" catches a
caller-taking write mis-filed as a read. Asserting "every `write` arm takes a
caller" is what stops the inverse error the record itself made: a real,
caller-taking write arm quietly filed anywhere other than the write set. The
residual is therefore an arm that **writes but takes no caller** — a handler
mutating store or provider state with no caller to attribute it to. The
codebase has no such arm today (attribution is DL-050-mandatory on every
write path), and the record notes the shape so a future arm of it is
recognized as needing a gate the signature cannot infer. The earlier draft
asserted the residual was the reverse shape — "a write arm that both takes no
caller AND writes" was described as absent while "takes a caller and writes"
was assumed to imply write-classification; the subscribe arms are precisely
the counterexample.

### A4 — the Dogfood/Beta tier switch

Enforcement is a serve-config bit, not a build variant:

- `ForgeConfig.EnforceScopes bool` (beside `SeedRepos`/`Poll` on the
  `ForgeConfig` struct in `go/server/serve.go`), default **false** = today's
  Dogfood posture, zero behavior change for existing deployments — the same
  all-optional posture the `ForgeConfig` doc comment already documents
  ("All-optional: the board lane is off (no App config) and writes are off
  (no write secrets) unless the operator opts in").
  Whether default-false survives freeze is **OQ-1 (load-bearing, deferred to
  Matt)**: on a Beta deployment an unset flag fails OPEN — enforcement
  silently off on the exact tier the ruling names.
- `buildForgeWriteService` (`go/server/serve.go`) threads it into
  `newForgeService`, which stores it on `forgeService` (a new
  `enforceScopes bool` field beside `now` — the `forgeService` struct in
  `go/server/forge.go`).
- When `EnforceScopes` is true and `ScopeGrants` is empty and the table is
  empty, startup logs a Warn (the `warnPartialForgeWriteSecrets` posture in
  `go/server/serve.go` — "diagnostic only, never fail-fast"): enforcement-on
  with zero grants means every write rejects,
  which is fail-closed and legal but probably an operator mistake.
- When `EnforceScopes` is true and NO GitHub App config is present (the
  PAT-only posture), startup logs a second loud Warn (the same
  `warnPartialForgeWriteSecrets` posture; App presence comes from
  `ForgeConfig.forgeWriteAppsConfigured` in `go/server/serve.go`): the
  forge-API leg enforces but the
  git-op leg (A5) is UNENFORCED — the container credential is a PAT that
  reaches whatever the PAT reaches. Beta-on-PAT must therefore never be
  silent — this is the OQ-6(ii) gap, disclosed not closed. The Warn is
  the drafted behavior; whether it should hard-fail startup instead is
  Matt's call at the OQ-6(ii) fork.
- Half-landed cross-check (the inverse window): when `EnforceScopes` is
  true AND App config IS present, a provision that goes out WITHOUT a
  narrowed credential (a static or absent `CredentialSource`, T5) logs a
  Warn naming the unenforced git leg — the same silent-half-landed shape
  OQ-1 guards on the flag itself. T4 carries the check.
- The Beta deployment profile sets `EnforceScopes: true`; there is no code
  fork between tiers, only config.

### A5 — git-operation scope: scope the credential, not the git call

**The gap.** A1-A4 gate the forge-API write path only. Agent git
clone/push/pull runs INSIDE the agent's rootless-podman container,
authenticated by a git `store` credential helper the Runner seeds at
provision time: `Workspace.CredentialSetupScript()` writes
`credential.helper "store --file=$h/.git-credentials"` into the agent's
`$HOME/.gitconfig` and a 0600 `$HOME/.git-credentials` line of the shape
`https://<user>:<token>@<host>` (`Workspace.CredentialSetupScript` in
`go/internal/runtime/workspace.go`; the
`Credentials{Host, Username, Token}` struct in the same file).
That credential is host-wide: one token line serves **every repo on the
host** the token itself can reach. So an agent whose forge-API writes to
repo X reject under A3 can still `git push` to repo X — the exact gap Matt
flagged. There is no server chokepoint to extend: the Runner never clones
for the agent ("launch must not run a git clone" — the assertion in
`TestLaunchOrdersStagesEgressBeforeCheckoutDir`,
`go/internal/runtime/agent_test.go`), and routing git traffic
through one would be a new proxy (rejected — see Alternatives).

**The mechanism.** Make the credential itself carry the scope: when GitHub
App config is present (the RIG-2732 / PR #634 posture — per Matt's W1 ruling
(RIG-2732), the GitHub App is THE credential for both read and write, so
there is one credential class to narrow rather than a read token and a write
token to keep in step), the token seeded into `$HOME/.git-credentials` is
a **GitHub App installation access token minted narrowed to exactly the
agent's workstream repo plus the account's `account_forge_scopes` GitHub
repo set** (the self-clone invariant, next paragraph). One allowlist plus
one invariant, two enforcement points: the A3 server chokepoint rejects
out-of-scope forge-API writes; the scope-narrowed credential makes an
out-of-scope `git push`/`git clone` fail at GitHub itself (404 for a
private repo outside the token's repo set, the desirable not-found shape
that aligns with the A3 not_found merge; 403 for an in-scope repo the
token's permissions do not cover) — no
in-container enforcement code, nothing the agent can tamper with from
inside its own container.

**The self-clone invariant (read vs write).** `account_forge_scopes` is a
WRITE allowlist, but a git credential also gates CLONE — a read. The
load-bearing correctness constraint: the agent MUST be able to clone/pull
its own workstream (spawn-target) repo, or provisioning succeeds and the
agent is dead on arrival — the Runner never clones for it ("launch must not
run a git clone" — `TestLaunchOrdersStagesEgressBeforeCheckoutDir`,
`go/internal/runtime/agent_test.go`); the
provision contract gives the container a git credential and lets "the agent
self-clone whatever it needs after launch". A5 therefore REQUIRES: the
agent's own workstream repo is always in git-op scope (clonable/pullable),
independent of the write allowlist; push stays write-gated — by the
installation token's `permissions` narrowing object and, on the forge-API
path, the A3 chokepoint; any repo beyond the workstream repo enters the
narrowed credential only via an `account_forge_scopes` grant.

**Unmet precondition — the server does not know the workstream repo.**
Minting a token narrowed to "the workstream repo" needs that repo as a
server-side input, and it is not one today: repo carriage was deliberately
REMOVED from provision (RIG-1527, Matt 2026-07-29 — "spawn/provision no
longer clone a repo for the agent ... the agent self-clones whatever it
needs after launch", the repo-carriage-removed comment inside
`ProvisionAgentWorkspaceRequest` in `proto/compass/v1/compass.proto`).
`ProvisionAgentWorkspaceRequest` carries no repo — its four fields are
`agent_handle` (1), `client_request_id` (2), `persona` (3), and `role` (4) —
`agent_accounts` has
no repo column (its `CREATE TABLE` in
`go/internal/store/migrations/0001_init.sql`), and
neither `StartAgentSessionRequest` nor `SpawnAgentRequest` carries one. So
the whole invariant — and the T4 mint's `workstreamRepo` argument — rests
on an input that must first be re-introduced (a provision/spawn field, an
`agent_accounts` column, or a store-side spawn-target record), which
REVERSES RIG-1527 and is Matt's call: **OQ-8**. Until OQ-8 resolves,
T4/T4.5/T5 are blocked on it, not only on #634's App landing. Zero or
insufficient grants at provision — the workstream repo cannot be put in
scope, e.g. it sits outside the App installation's own repo grant — FAILS
THE PROVISION LOUD: reject at provision time, never silently provision a
credential-less or wrong-scoped container (T4's mint error and T5's
fail-on-source-error agree on fail-loud).

**Read/write asymmetry — two consequences, both Matt's.** (1) Whether the
git-op read set is exactly "workstream repo + write set" or a distinct read
column is OQ-7. Narrowing clone to that set is a real REGRESSION against
RIG-1527's "self-clones whatever it needs": under the host-wide token today
an agent can clone any read-only dependency repo (a sibling library, a
reference repo) it will never write; under the narrowed token a clone of
any repo that is neither the workstream repo nor write-granted fails at
GitHub. OQ-7 carries that tradeoff as first-class. (2) The asymmetry with
OQ-4, stated honestly: forge-API reads stay ungated this slice while git
clone is scoped — because the git credential is one token gating both
directions, where the API path can gate writes alone.

**Load-bearing premise (verified against the GitHub REST docs, "Create an
installation access token for an app", 2026-08-26):**
`POST /app/installations/{installation_id}/access_tokens` accepts optional
`repositories` (names) / `repository_ids` body parameters — "the
installation access token cannot be granted access to repositories that the
installation was not granted access to", up to 500 repositories — plus a
`permissions` narrowing object. If this capability did not hold, the
recommendation here would change (one of the heavier Alternatives would be
back on the table); it does hold, and the whole A5 mechanism stands on it.
Two consequences the design must absorb:

- **Expiry:** installation tokens expire **one hour** after mint (same
  docs). The credential file is written once at provision
  (`AgentRuntime.installCredentials` in `go/internal/runtime/agent.go`, feeds
  the script over the container exec channel), so the token MUST be re-minted
  and re-applied before expiry. The server owns `expiresAt` (it minted the
  token), so refresh is a **server-driven push**: the server re-mints on
  margin and pushes the new token to the Runner over the T4.5 wire, and the
  Runner re-applies it via the `installCredentials` exec path — with
  retry-with-backoff, keep-old-token-on-failure, and an atomic file rewrite
  (T5, refresh hardening). A ~1h token is a NEW liveness dependency today's
  static PAT does not have; T5 states the availability posture explicitly.
  A long-lived agent session sees a rotating token, which is a security
  improvement over today's static PAT line, not a regression. The
  server-driven re-mint needs per-live-container state the board projection
  does not provide (it retains only `{state, account}` per live session with
  lifecycle GC deferred — the `sessions map[string]sessionEntry` field and the
  `sessionEntry` struct in `go/internal/board/projection.go` — no
  `expiresAt`/host/workstreamRepo, so it cannot source credential liveness):
  a durable record of `(container_name, account, host,
  workstreamRepo, expiresAt)` per live container plus a margin-driven
  scheduler that reads it, torn down on container stop. T4.5 owns that
  registry (its refresh push has nowhere to key from otherwise).
- **Revocation residual, accepted and stated:** the grant set is baked into
  the token at mint time, while §A3's chokepoint re-reads
  `account_forge_scopes` per call. So `RevokeForgeScope` takes effect on the
  forge-API leg immediately and on the git leg only at the next margin
  refresh — a revoked grant keeps push access to that repo for up to the
  token's remaining validity (≤1h). A revoke-triggered re-mint would close
  it; this record accepts the window rather than specifying one, and T4.5's
  scheduler is where it would land if Matt wants it closed.
- **Wildcard grants:** a `repo = '*'` row (A1) means the whole coordinate —
  the mint then OMITS the `repositories` field, yielding a token with the
  installation's full repo access. Exact-repo grants list exactly those
  repos. Repos granted in `account_forge_scopes` but outside the App
  installation's own grant simply don't widen the token (GitHub clamps to
  the installation), which is fail-closed in the right direction.

**Seam and lane ownership.** The mint is a **server-side** concern: the App
private key and the `account_forge_scopes` table both live with the server
(the store check is A1's `HasForgeScope` table; the mint needs a sibling
list method, T4). The **credential delivery** is a Runner concern: the
production `configSpecBuilder` builds `runtime.Workspace` with `Credentials`
deliberately unset today, and its package comment reserves exactly this
seam — "the per-agent-account credential and egress derivation that later
tiers add plugs into the same SpecBuilder seam without changing Provision"
(the package comment on `go/internal/runner/spec.go`; the builder is
`configSpecBuilder.BuildSpec` in the same file, whose returned
`runtime.Workspace` sets only `CheckoutDir`/`HomeDir`/`UID`). The
provision flow already routes Client → Server → RunnerHub → Runner (the
`ProvisionAgentWorkspace` rpc comment in
`proto/compass/v1/compass.proto`) — but the Server and the Runner
are separate processes, and `ProvisionAgentWorkspaceRequest` carries NO
credential field today (its four fields are `agent_handle`,
`client_request_id`, `persona`, `role` — `proto/compass/v1/compass.proto`;
the production spec
builder fills `Workspace` from Runner-local defaults,
`configSpecBuilder.BuildSpec`). The
minted token therefore needs an explicit WIRE DELTA to cross the process
boundary: T4.5 names it. T4 (server: mint) → T4.5 (server + proto:
transport + refresh push) → T5 (runner: install + refresh application)
split on precisely these lines; all three must land for the leg to
enforce — flagged to compass-server and compass-runner.

**Sibling credential surface.** The gh-CLI credential
(the `GHCredentials{Host, Token}` struct routed to `~/.config/gh/hosts.yml`,
`go/internal/runtime/secrets_materialize.go`, populated from
`SecretGH` secrets in `SecretMaterializer.Install`'s
`case secrets.SecretGH:` arm in the same file) is the same
credential class on a second file. A deployment that seeds a broad PAT
there re-opens the gap A5 closes on the git path. The hosts.yml surface
takes the same narrowed token when the App path is active, and the
PAT-fallback posture (OQ-6) governs it identically — but note it is a
SEPARATE write path (`GHHostsScript` driven from the `SecretGH` arm of
`SecretMaterializer.Install`), NOT reachable from
the `installCredentials` `.git-credentials` path. Because they are separate
paths, T5 materializes hosts.yml with the narrowed token at BOTH provision
and refresh (not refresh alone): wiring only `.git-credentials` at provision
would leave hosts.yml holding the static PAT until the first refresh, and a
refresh that rotated only `.git-credentials` would leave it holding an
expired token within the hour — either way breaking every gh-CLI op. T5
owns both surfaces at both moments.
(The hosts.yml surface needs no username posture line of its own: it
carries a bare `oauth_token` with no username field — `GHHostsScript` in
`go/internal/runtime/secrets_materialize.go` emits only
`oauth_token: <token>` per host block — so an installation
token drops in as-is; the git-credentials line is the surface that needs
the `x-access-token` username — T5.)

**PAT fallback.** When NO GitHub App is configured (the static-PAT path),
per-account mint-time narrowing is unavailable. Be precise about which PAT:
a **classic** PAT is not repo-narrowable at all; a **fine-grained** PAT IS
repo-narrowable, but only statically at creation time — one fixed repo set
per token, not per-account, not re-derivable from `account_forge_scopes` at
mint. Under PAT-only config the git-op scope is therefore (a) unenforced —
today's posture, credential reaches whatever the PAT reaches — (b) enforced
via one of the heavier rejected mechanisms, or (c) coarsely bounded by a
fine-grained PAT statically scoped at creation to the deployment's
granted-repo union (enforceable without an App, partially honoring
"unconditional", but deployment-wide, not per-account). This record designs
against (a) with a LOUD startup Warn (§A4) so Beta-on-PAT is never
silent — but that is Matt's freeze-gate call, surfaced as **OQ-6(ii)**
together with the mechanism fork itself.

### Alternatives considered

- **Prompt-level-only (status quo A8).** Rejected for Beta by ruling: the
  tool prompt's capability matrix is advice to a model, not authz; a
  hallucinated/injected `repo` sails through (the trust-model paragraph of
  the `go/server/forge.go` header comment records attribution only).
- **Repo in the credential key** (per-repo credentials in
  `forgeProviderRegistry`). Rejected: it reverses the provider+host
  coordinate key recorded in the `forgeCoordinate` commentary in
  `go/server/forge.go` ("A repo does NOT enter the key — one credential pair
  serves every repo on a coordinate"), multiplies secrets per repo, and still
  needs an account→credential map — strictly more moving parts than a scope
  row. (Note for the next reader: that comment attributes the provider+host
  key to DL-091, and the attribution looks wrong — DL-091 in
  `docs/designs/DECISIONS.md` is the Compass-issue archival transition, not a
  forge-credential-key decision. The KEY SHAPE the comment describes is real
  and is what this record relies on; the id is not. Do not propagate the
  DL-091 reference out of that comment.)
- **Reuse `forge_repo_subscriptions` as the allowlist.** Rejected: it is the
  board's poll target set, per-deployment not per-account (its `CREATE TABLE`
  and preceding comment in `go/internal/store/migrations/0001_init.sql`:
  "The board's repo-level webhook targets (OQ-C)"); coupling ingestion
  targets to write authz makes
  "stop polling a repo" silently mean "revoke writes", and gives every
  account identical scope — no blast-radius reduction between agents of
  different owners.
- **Per-agent-only grants (no owner inheritance).** Deferred, not rejected:
  the schema (keyed on bare `account_id`) admits it additively; MVP checks
  agent-or-owner because grants-per-owner match the standing "an agent acts
  within its owner's space" ruling (the `requireGroupCreateAuthz` doc comment
  in `go/internal/store/authz.go`) and keep the grant set
  administrable. OQ-3.
- **A fused `resolveWriteTarget` helper (resolveTarget + scope gate as one
  call every write arm must use).** Rejected as the anti-bypass choke: the
  create arms gate AFTER the F3 dedup while the comment/review and subscribe
  arms gate
  right after `resolveTarget` (§A3), so one fused call cannot sit in one
  place — it would need two shapes or a mode flag, which is the per-arm
  wiring problem wearing a helper's name. The file also already prefers the
  explicit per-arm parallel over extracted helpers on these very arms ("a
  closure-extracted helper reads worse than the explicit parallel" — the
  `//nolint:dupl` rationale on `commentOnIssue` in `go/server/forge.go`).
  The bypass risk is carried by the §A3 write-arm
  exhaustiveness test instead, which catches an ungated or unclassified new
  arm at the oneof-descriptor level.
- **Connect `PermissionDenied` instead of in-band.** Rejected: violates the
  frozen in-band/Connect split (the "in-band vs Connect split" paragraph of
  the `go/server/forge.go` header comment) and un-merges
  forbidden-from-not-found, giving an injected prompt a probe oracle.
- **A custom git credential-helper binary in the container (git-op leg).**
  A helper that consults the allowlist per-repo on every git operation and
  refuses out-of-scope remotes. Rejected: it enforces from INSIDE the
  container the agent controls — the agent can bypass its own `.gitconfig`
  (`git -c credential.helper=…`, or read the raw token if the helper caches
  one), so it is advice, not authz, unless the helper also holds the only
  credential — at which point it needs a callback channel to the server to
  fetch per-repo tokens, i.e. it converges on the A5 narrowed-token mint
  with a new binary, a new in-container protocol, and a new attack surface
  added on top. Strictly heavier for the same result.
- **A server-side git proxy (git-op leg).** Route all agent git traffic
  through a scope-enforcing endpoint (a smart-HTTP proxy fronting the
  forge). Rejected: it is a new always-on network service in the data path
  of every clone/push/pull — availability, TLS, streaming pack-protocol
  passthrough, and egress-policy surgery (agent egress currently allows the
  forge host directly — `MustAllowEgress("github.com")` in the
  `specWithCreds` fixture, `go/internal/runtime/agent_test.go`) — to
  re-derive a rejection GitHub already produces natively when the credential
  is narrowed. The proxy also still needs the credential downstream, so it
  adds a hop without removing the token. Heaviest option, no additional
  enforcement over A5.
- **Per-repo SSH deploy keys (git-op leg).** One key pair per repo, seeded
  per grant. Rejected: per-repo key sprawl (a key minted, stored, and
  rotated per grant row), the agent credential path is HTTPS
  (`https://<user>:<token>@<host>`, built in
  `Workspace.CredentialSetupScript`, `go/internal/runtime/workspace.go`)
  not SSH, and deploy keys are per-REPO not per-ACCOUNT — two agents with
  different grant sets on one repo would share a key, so the blast-radius
  boundary lands in the wrong place.
- **Fine-grained static PAT (git-op leg).** A fine-grained PAT statically
  repo-scoped at creation to the deployment's granted-repo union. Not a
  full alternative to the installation-token mint — it is deployment-wide
  and fixed at creation, so it cannot track per-account grants — but it is
  the honest middle option when no App is configured; carried as
  OQ-6(ii)(c) rather than dismissed.

## Plan

### T1 `[compass-server]` — store: `account_forge_scopes` table + scope check

Schema change folded into `0001_init.sql`, per the fold-as-it-accretes
convention that file states for itself, with the A1 DDL — including the
`tenant_id` column and its position FIRST in the PRIMARY KEY, mirroring
`forge_repo_subscriptions`.

**First, re-confirm the premise that licenses the fold**: `0001_init.sql`
records "zero users, zero deployed databases", and folding is correct only
while that holds. If a deployed database exists by then, switch to a new
numbered migration carrying the `CREATE TABLE` **and** its own explicit
`ALTER TABLE … ENABLE`/`FORCE ROW LEVEL SECURITY` +
`CREATE POLICY tenant_isolation` statements inline, per §A1.

**The same change MUST enroll the table in row-level tenant isolation**: add
`'account_forge_scopes'` to the `tenant_tables text[]` array driving the
`ENABLE`/`FORCE ROW LEVEL SECURITY` + `CREATE POLICY tenant_isolation` `DO`
loop in that file. Folding both halves into one migration is what makes the
enrollment execute at all — a separate `000N_*.sql` creating the table would
leave the array edit unapplied on every existing database (`Store.migrate`
skips already-recorded versions), shipping a `tenant_id` column with no policy
over it: an authz table that only LOOKS tenant-isolated. Enrollment is a named
acceptance criterion of this task, with a pgtest asserting it (a row written
under tenant A is invisible to a `compass_app` connection scoped to tenant B,
and a write with no `compass.tenant_id` GUC is rejected — the fail-closed
shape the migration's own RLS commentary specifies).

Store surface in a new `go/internal/store/forge_scopes.go`:

Interfaces:

```go
// ForgeScope is one write-scope grant row.
type ForgeScope struct {
    AccountID AccountID
    Provider  ForgeProvider
    Host      string
    Repo      string // "*" grants the whole coordinate
}

// GrantForgeScope inserts idempotently (ON CONFLICT DO NOTHING); GITHUB repo
// lowercased; zero/empty fields -> ErrInvalidArgument.
func (s *Store) GrantForgeScope(ctx context.Context, g ForgeScope) error

// RevokeForgeScope deletes one grant; unknown row -> ErrNotFound.
func (s *Store) RevokeForgeScope(ctx context.Context, g ForgeScope) error

// HasForgeScope reports whether account (or, for an agent, its owning user)
// holds a grant for (provider, host, repo) — exact repo or '*'. repo is
// normalized with the SAME provider-aware fold GrantForgeScope applies
// (GITHUB lowercased, LINEAR team key preserved) before comparison, so
// grant and check always agree on case (§A1).
func (s *Store) HasForgeScope(ctx context.Context, account AccountID, provider ForgeProvider, host, repo string) (bool, error)
```

Tests: pgtest suite (grant/revoke idempotency, agent-inherits-owner, `'*'`
wildcard, case fold on BOTH sides — a mixed-case GITHUB query repo matches a
lowercased grant, a LINEAR team key matches verbatim — FK RESTRICT) mirroring
`forge_cursors_pgtest_test.go`'s shape.

### T2 `[compass-server]` — chokepoint: `requireForgeScope` in the write arms

Interfaces:

```go
// the forgeStore interface (go/server/forge.go) gains:
HasForgeScope(ctx context.Context, account store.AccountID, provider store.ForgeProvider, host, repo string) (bool, error)

// the forgeService struct (go/server/forge.go) gains: enforceScopes bool
// newForgeService (go/server/forge.go) gains the flag:
func newForgeService(st *store.Store, issueBrd *board.IssueProjection, providers *forgeProviderRegistry, enforceScopes bool) *forgeService

func (s *forgeService) requireForgeScope(ctx context.Context, caller store.AccountID, rf resolvedForge, repo string) *compassv1internal.ForgeCallError
```

Wire `requireForgeScope` per the §A3 asymmetry, all in `go/server/forge.go`:
in the create arms AFTER the F3 dedup memo check (`createIssue` and
`createPullRequest`, each immediately after their `s.dedup(...)` call and
before `s.resolveIdentity(...)`) so a memo hit still
returns the recorded coordinate writing nothing; in the comment/review arms
directly after `resolveTarget` (`commentOnIssue`,
`commentOnPullRequest`, `submitReview` — no dedup to
order against, F3 is create-only per the `record` doc comment); and in
`subscribeForge` directly after its existing `resolveTarget` call and before
`subscribeToStoreKind` (§A3 — it is a coordinate-keyed store write).
`unsubscribeForge` takes NO `requireForgeScope` call: it carries a
subscription id and no coordinate, and `Store.DeleteAgentForgeSubscription`
already scopes the delete to the calling agent (§A3). T2 asserts that
caller-scoping by test rather than adding a coordinate lookup that would
authorize nothing.
Update the `go/server/forge.go` header comment: the A8 posture line ("the
caller is recorded for attribution but NO scope rejection ships") becomes
"scope enforcement per RIG-2679, gated by enforceScopes".

Tests (default lane, red first): extend `fakeForgeStore`
(`go/server/forge_test.go`) with a scope set; per write arm assert (a)
enforcement-off passes with zero scope rows, (b) enforcement-on +
out-of-scope rejects with the byte-fixed in-band `not_found` (byte-identical
to the 403 ≡ 404 flatten text `mapForgeError` emits, §A3) and the provider
fake records **zero
calls** and no DL-055 row lands, (c) enforcement-on + exact-repo and `'*'`
grants pass, (d) store fault maps via `storeForgeError`, (e) read arms
unaffected, (f) a create whose `client_request_id` has a memo hit returns
the recorded coordinate with enforcement ON and ZERO grants (the F3 retry
contract, §A3), (g) `subscribeForge` under enforcement-on + zero grants
rejects with the same byte-fixed `not_found` and lands NO
`agent_forge_subscriptions` row, and (h) `unsubscribeForge` against another
agent's subscription id returns the store's not_found (its caller-scoping,
unchanged by this slice). Plus the §A3 write-arm exhaustiveness test over the
`ForgeCallRequest` oneof descriptors (an unclassified or ungated new arm
turns it red), paired with the §A3 signature cross-check in its
biconditional form — an arm is `read`-classified iff its handler takes no
`caller store.AccountID`, and every `write`-classified arm's handler takes
one — so BOTH a caller-taking write mis-filed as a read and a real write arm
omitted from the write set redden the test. E2E: one whole-wire case in
`go/server/forge_e2e_pgtest_test.go` over
the `newForgeE2EWire` scaffold (same file) proving
the rejection shape end to end against real Postgres.

### T3 `[compass-server]` — serve assembly: flag, seed, warn

Interfaces:

```go
// the ForgeConfig struct (go/server/serve.go) gains:
//   EnforceScopes bool     // Beta: true; absent/false = Dogfood defer
//   ScopeGrants   []string // "handle=provider:host/repo", repo may be "*"
// buildForgeWriteService (go/server/serve.go) passes cfg.Forge.EnforceScopes
// to
// newForgeService and reconciles ScopeGrants before returning:
func reconcileForgeScopeSeed(ctx context.Context, st *store.Store, grants []string) error
```

Seed semantics mirror `reconcileForgeSeed` (`go/server/serve.go`):
bootstrap-only `GrantForgeScope` per entry, handle resolved to `account_id`
via the store, bad entry fails startup. Warn on enforcement-on + empty grant
set (A4). CLI flags/env plumbed wherever `SeedRepos`/`Poll` already are.

Tests: config-parse + seed-reconcile unit tests beside
`go/server/serve_forge_test.go`; a pgtest reconcile case in
`go/server/serve_forge_pgtest_test.go`, following the shape of its existing
`TestBoardIngestionDisabledWarnsOnEnabledRows` (a real-Postgres store, a
seeded coordinate, an assertion on the reconciled rows).

### T4 `[compass-server]` — narrowed installation-token mint from the allowlist

Depends on the RIG-2732 / PR #634 App landing (App id, private key,
installation id in server config — that record owns their shape). Gated on
App config presence: no App, no mint (the PAT fallback posture, OQ-6).

Interfaces:

```go
// ListForgeScopeRepos returns the exact-repo grants held by account (or,
// for an agent, its owning user) on (provider, host), and whether a '*'
// wildcard grant exists. Repos come back in the stored (GITHUB-lowercased)
// fold. Empty + no wildcard means zero grants.
func (s *Store) ListForgeScopeRepos(ctx context.Context, account AccountID, provider ForgeProvider, host string) (repos []string, wildcard bool, err error)

// MintScopedInstallationToken mints a GitHub App installation access token
// narrowed to the union of the agent's workstream repo and the account's
// grant set on host: wildcard -> the repositories field is omitted
// (installation-wide token); otherwise repositories lists exactly
// workstreamRepo plus the granted repos. GitHub's `repositories` field
// takes bare NAMES resolved relative to the installation owner, which is
// only sound when the grant's owner equals the installation owner; a grant
// like `otherorg/thing` would alias to `installationOwner/thing`. The mint
// therefore either restricts App-mint grants to the installation owner or
// uses `repository_ids` (the API also accepts ids) resolved from the stored
// owner/name grants. workstreamRepo is the §A5 self-clone invariant repo —
// ALWAYS in scope so the agent can clone/pull its own workstream repo; push
// stays write-gated by the token's permissions object + the A3 chokepoint.
// Its SOURCE is unresolved (RIG-1527 removed repo carriage from provision):
// this argument is blocked on OQ-8. Zero grants therefore still mint — a
// token narrowed to exactly the workstream repo. The workstream repo
// unreachable (outside the App installation's own grant) or the mint
// failing -> error, and the caller (T5's server-backed CredentialSource)
// FAILS THE PROVISION LOUD — never a silent credential-less or
// wrong-scoped container.
// Returns the token and its GitHub-side expiry (~1h).
func (m *ForgeAppTokenMinter) MintScopedInstallationToken(ctx context.Context, account store.AccountID, host, workstreamRepo string) (token string, expiresAt time.Time, err error)
```

Half-landed cross-check (§A4): the server provision path asserts that when
`EnforceScopes` is true and App config is present, every provision carries
a narrowed credential; a provision going out on a static or absent
`CredentialSource` logs a Warn naming the unenforced git leg.

Tests (red first): unit tests against a fake GitHub token endpoint — (a)
exact grants produce a `repositories` body listing exactly those names plus
the workstream repo, (b) a `'*'` grant omits the field, (c) zero grants
produce a `repositories` body of exactly the workstream repo (the
self-clone floor), (d) the GITHUB-lowercase fold from A1 is what reaches
the request body, (e) a workstream repo the installation cannot grant
errors (fail-loud), (f) the half-landed cross-check Warn fires on a
static-source provision under enforcement-on + App-present, (g) a grant
whose owner differs from the installation owner resolves by id (or is
rejected), never aliased to a same-name repo under the installation owner;
pgtest for
`ListForgeScopeRepos` (agent-inherits-owner, wildcard flag, fold) beside
the T1 suite.

### T4.5 `[compass-server, proto]` — credential wire delta: provision carry + refresh push

Server and Runner are separate processes (Client → Server → RunnerHub →
Runner — the `ProvisionAgentWorkspace` rpc comment in
`proto/compass/v1/compass.proto`) and
`ProvisionAgentWorkspaceRequest` carries no credential field today
(its four fields are `agent_handle`, `client_request_id`, `persona`, `role`
— `proto/compass/v1/compass.proto`); the production spec builder
fills `Workspace` from Runner-local defaults
(`configSpecBuilder.BuildSpec` in `go/internal/runner/spec.go`). The minted
token needs an explicit
wire path across the boundary: this task owns it.

Interfaces:

```proto
// ProvisionAgentWorkspaceRequest gains the minted credential.
// SERVER-AUTHORITATIVE like persona/role (fields 3 and 4 of
// ProvisionAgentWorkspaceRequest, whose comments state the Server
// "overwrites any client-supplied value"): the
// Server populates it on the provision path and overwrites any
// client-supplied value. token is debug_redact per the IssueToken
// redaction convention (the RevokeToken rpc comment: the plaintext "is never
// logged (debug_redact)"); the field-level convention is
// RevokeTokenRequest.token and SetSecretRequest.value, both
// `[debug_redact = true]` in compass.proto.
message WorkspaceCredential {
  string host = 1;
  string username = 2;        // "x-access-token" for installation tokens (T5)
  string token = 3 [debug_redact = true];
  int64 expires_at_unix = 4;  // 0 = never (static PAT)
}
// ProvisionAgentWorkspaceRequest: WorkspaceCredential credential = 5;

// Refresh: a dedicated Server→Runner push on the hub control channel — a
// new RunnerHub-relayed RefreshWorkspaceCredential message keyed by
// container_name, carrying the same WorkspaceCredential.
```

Refresh is **server-driven push**, not Runner-pull, and the choice is
load-bearing: the server owns `expiresAt` (it minted the token, T4), owns
the re-mint (the App private key and the `account_forge_scopes` allowlist
are both server-side, §A5), and already holds the Server → RunnerHub →
Runner control path — a push adds one message on an existing channel,
where a Runner-pull would add a new Runner→Server RPC surface plus
per-Runner refresh scheduling against an expiry the Runner only knows
second-hand. The Runner applies a pushed credential via the T5
`installCredentials` re-exec.

**Refresh-scheduler state (server-side).** The push needs a source to key
from. T4.5 adds a durable per-live-container registry —
`(container_name, account, host, workstreamRepo, expiresAt)`, written at
provision when the token is minted, evicted on container stop — and a
margin-driven loop (~T-10min before `expiresAt`) that re-mints via T4 and
pushes. It cannot fall out of the board projection (the `sessionEntry` struct
and `Projection.sessions` map in `go/internal/board/projection.go`), which
retains only
`{state, account}` per live session (lifecycle GC deferred) and carries no
`expiresAt`/host/workstreamRepo; credential liveness needs its own durable
store, owned by compass-server.

Tests (red first): a server provision-path unit test (the credential field
is populated server-side and overwrites a client-supplied value; redaction
asserted the same way the IssueToken token field's is); a hub-relay test
that a refresh push reaches the Runner keyed by `container_name`; a
scheduler test that a live-container record fires a re-mint+push on margin
and is evicted on container stop.

### T5 `[compass-runner]` — credential-provision wiring + hardened refresh

Thread the minted token into the reserved SpecBuilder credential seam
(the package comment on `go/internal/runner/spec.go`;
`Workspace.Credentials` today unset in the production builder,
`configSpecBuilder.BuildSpec` in the same file) so the provisioned
container's `$HOME/.git-credentials` line
(`Workspace.CredentialSetupScript` in `go/internal/runtime/workspace.go`)
carries the narrowed
token instead of a static PAT. The credential arrives on the T4.5 wire (the
provision field at provision; the refresh push thereafter). Both the token's
credential surfaces MUST carry the narrowed token from t=0, not only after
the first refresh: at **provision** the Runner materializes BOTH the
`.git-credentials` file (via the `AgentRuntime.installCredentials` exec path,
`go/internal/runtime/agent.go`) AND the gh-CLI
`~/.config/gh/hosts.yml` `oauth_token` (via the `GHHostsScript` write, and
the `SecretGH` arm of `SecretMaterializer.Install`, both in
`go/internal/runtime/secrets_materialize.go`) from the
T4.5 `WorkspaceCredential` — constructing `GHCredentials{Host, Token}` from
it and running `GHHostsScript` alongside `CredentialSetupScript`. Wiring
only `.git-credentials` at provision would leave hosts.yml holding the
static `SecretGH` PAT (or absent) until the first refresh at ~T-10min — a
~50-minute window reopening the exact broad-token gap A5 closes, on the
gh-CLI surface. **Refresh** re-materializes the same two surfaces
atomically with the re-minted token: they are separate write paths
(`installCredentials` never touches hosts.yml), so a refresh that rotated
only `.git-credentials` would leave hosts.yml holding an expired token and
break every gh-CLI op within the hour. Both the provision and refresh
`GHHostsScript` calls MUST pass the FULL current gh-host credential set, not
the single rotated GitHub credential: `GHHostsScript` rewrites the entire
`hosts.yml` in one whole-file `mv` (its `t="$f.tmp.$$"` … `mv "$t" "$f"`
tail), so
passing only the rotated credential would clobber any co-resident host block
(e.g. a GitHub Enterprise host also seeded via `SecretGH`). When the set
carries two entries for the SAME host — a static `SecretGH` PAT and the
narrowed installation token for `github.com` under App-active — the merge
MUST let the narrowed token win: `GHHostsScript` collapses same-host
duplicates last-wins (its `tokenByHost` map plus the `order` slice — "two
credentials naming the same host must collapse to a single block
(last wins)"), so the narrowed
token is written last, or the static PAT is dropped from the set before the
write. Otherwise hosts.yml silently re-holds the broad PAT — the exact gap
this wiring closes.

**Username.** git-over-HTTPS with an installation token authenticates as
username **`x-access-token`**; the seeded line is
`https://<user>:<token>@<host>` built from `Credentials.Username`
(`Workspace.CredentialSetupScript` in
`go/internal/runtime/workspace.go`), so the server-backed source
returns `Credentials{Username: "x-access-token", …}`. The gh hosts.yml
surface needs no username line: it carries a bare `oauth_token` with no
username field (`GHHostsScript` emits only `oauth_token: <token>` per host
block, `go/internal/runtime/secrets_materialize.go`), so
the same token drops in as-is (§A5 sibling-surface note).

**Refresh liveness (availability).** A ~1h token is a NEW liveness
dependency: the mint or GitHub's token endpoint down at refresh time kills
every live agent's git within the hour, where today's static PAT never
expires. Posture: (1) refresh with MARGIN — the server re-mints at ~T-10min
before `expiresAt`, never at expiry; (2) retry-with-backoff on mint
failure; (3) the old still-valid token STAYS IN PLACE until a new one
lands — a failed refresh never truncates or clears the credential file.
Accepted residual, stated explicitly: a multi-request git operation
straddling the actual expiry of a token whose refresh is still failing sees
mid-operation 401s — a bounded in-flight-expiry window this design accepts.

**Refresh atomicity (race).** `CredentialSetupScript` rewrites
`.git-credentials` by truncate-then-write (`cat > "$h/.git-credentials"` in
`Workspace.CredentialSetupScript`,
`go/internal/runtime/workspace.go`) — a git process reading
mid-rewrite sees a truncated credential and fails auth transiently. The
sibling `GHHostsScript` already solves this shape with tmp-file +
`chmod 600` + atomic `mv`
(its `chmod 600 "$t"` / `mv "$t" "$f"` tail,
`go/internal/runtime/secrets_materialize.go`). T5 makes the
refresh rewrite atomic the same way — amend `CredentialSetupScript` (write
`$f.tmp.$$`, chmod 600, `mv`), which also hardens the first seed for free.

Interfaces:

```go
// CredentialSource resolves the per-account workspace credential at
// provision. The server-backed implementation is fed by the T4.5 wire
// (the minted token + expiry carried on the provision message and pushed
// on refresh); the static implementation wraps a configured PAT (today's
// posture, PAT fallback). A source error FAILS THE PROVISION LOUD — never
// a silent credential-less container (§A5; the mint side agrees, T4).
type CredentialSource interface {
    // WorkspaceCredentials returns the credential to seed plus when it
    // must be refreshed (zero time = never, the static-PAT case).
    WorkspaceCredentials(ctx context.Context, account string) (creds *runtime.Credentials, refreshAt time.Time, err error)
}

// configSpecBuilder gains the source; BuildSpec populates
// Workspace.Credentials from it. Refresh application: on a T4.5 refresh
// push the Runner re-invokes the installCredentials exec path on the live
// container with the new token; a refresh that fails or never arrives
// past refreshAt retries with backoff and leaves the previous credential
// file untouched.
func NewConfigSpecBuilder(defaults SpecDefaults, creds CredentialSource) (SpecBuilder, error)
```

Tests (red first): spec-builder unit tests (credential populated from the
source; static source keeps today's behavior byte-for-byte; source error
fails provision LOUD, never provisions credential-less silently when a
source is configured); a provision test asserting BOTH surfaces carry the
narrowed token at t=0 — the `.git-credentials` line AND the hosts.yml
`oauth_token` — so an App-active provision never leaves hosts.yml on the
static PAT; a refresh test on the fake runtime asserting a second
credential-install exec lands before the deadline and the rewritten
`.git-credentials` carries the new token (the `fakeRuntime.callsSnapshot()`
pattern used by `TestLaunchOrdersStagesEgressBeforeCheckoutDir`,
`go/internal/runtime/agent_test.go`); a failed-refresh
test asserting the old credential file survives byte-for-byte; a script
test asserting the credential rewrite goes through tmp + `chmod 600` +
`mv` (the `GHHostsScript` shape in
`go/internal/runtime/secrets_materialize.go`); a
refresh test asserting the hosts.yml `oauth_token` rotates to the new token
alongside `.git-credentials` (both surfaces, one refresh); and a
multi-gh-host test asserting a refresh that rotates the GitHub host
PRESERVES a co-resident host block (`GHHostsScript`'s whole-file rewrite is
fed the full host set, not the single rotated credential).

### Tasks

- [ ] T1 — `account_forge_scopes` migration (`tenant_id` first in the PK) +
  `'account_forge_scopes'` added to the `tenant_tables[]` RLS enrollment +
  `Grant/Revoke/HasForgeScope` store methods + pgtest suite including the
  tenant-isolation case.
- [ ] T2 — `requireForgeScope` gate in the six coordinate-keyed write arms
  (the five forge writes, post-dedup on creates, plus `subscribeForge`) +
  the `unsubscribeForge` caller-scoping assertion + `forgeStore` extension +
  fake + default-lane, exhaustiveness, signature-cross-check,
  and e2e tests + header-comment update.
- [ ] T3 — `ForgeConfig.EnforceScopes`/`ScopeGrants` + seed reconcile + warn +
  assembly wiring + tests.
- [ ] T4 — `ListForgeScopeRepos` store method + `MintScopedInstallationToken`
  narrowed-token mint (App-config-gated, wildcard-aware, workstream repo
  always in scope, fail-loud on an unreachable workstream repo, grant/
  installation owner reconciled by id not bare-name alias) +
  half-landed cross-check Warn + fake-endpoint and pgtest suites. Depends
  on RIG-2732 / #634 App config AND on OQ-8 (the `workstreamRepo` input
  source RIG-1527 removed).
- [ ] T4.5 — credential wire delta: `WorkspaceCredential` on
  `ProvisionAgentWorkspaceRequest` (server-authoritative, `debug_redact`) +
  server-driven refresh push on the hub control path + the per-live-container
  refresh-scheduler registry `(container_name, account, host, workstreamRepo,
  expiresAt)` + tests. Depends on RIG-2732 / #634 App config AND on OQ-8.
- [ ] T5 — `CredentialSource` seam in the Runner spec builder + provision
  wiring (fail-loud) + `x-access-token` username + hardened pre-expiry
  refresh (margin, backoff, keep-old-token-on-failure, atomic tmp+`mv`
  rewrite) re-materializing BOTH the `.git-credentials` and hosts.yml
  surfaces via the `installCredentials` / `GHHostsScript` paths + tests.
  Depends on OQ-8.

## Ledger delta

Proposed row (id assigned by the driver at freeze — the record must not
hardcode it: the driver re-derives the next free id from
`docs/designs/DECISIONS.md` at freeze time, since any max recorded here goes
stale the moment another record merges), Comms & tools
section:

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-NNN | Forge scope enforcement (the deferred A8) is ONE per-account allowlist with TWO enforcement points. (1) Server chokepoint: a new `account_forge_scopes` table (agent-or-owning-user grant, exact repo or `'*'` per coordinate, provider-aware case fold applied identically at grant and check, seeded declaratively via `ForgeConfig.ScopeGrants` + owner grant methods, never agent-self-granted) checked by `requireForgeScope` in every coordinate-keyed write arm of `ExecuteForgeCallAsAccount` (the five forge writes plus `subscribeForge`; `unsubscribeForge` is caller-scoped by id in the store) — after coordinate resolution, and on the create arms after the F3 idempotency-memo check so a memo-hit retry (which writes nothing) is never rejected — before any provider call, rejecting out-of-scope targets as the in-band `ForgeCallError{code:"not_found"}` byte-identical to the provider-403/404 flatten text (the comms not-found/forbidden merge; never a Connect error), guarded against future ungated arms by a write-arm exhaustiveness test over the oneof descriptors, gated by `ForgeConfig.EnforceScopes` — false for Dogfood (single trust domain, today's posture preserved), MANDATORY true for Beta regardless of the RIG-2682 account model; the flag's DEFAULT direction (fail-open vs fail-closed) is OQ-1, deferred to Matt. (2) Git-op leg (GitHub-only — Linear has no git): when GitHub App config is present (RIG-2732), the agent container's `~/.git-credentials` token is a GitHub App installation access token minted narrowed via the API's `repositories` field to exactly the account's `account_forge_scopes` GitHub repo set, so an out-of-scope clone/push/pull fails at GitHub itself; PAT-only deployments keep today's unenforced git-op posture (the mechanism + PAT-fallback fork is OQ-6, deferred to Matt) | Proposed | [forge scope enforcement §Approach](server/compass-forge-scope-enforcement/design.md#approach) |

Ledger-impact: adds one row (Comms & tools); refines the A8 no-scope posture
DL-200 inherited (the trust-model paragraph of the `go/server/forge.go`
header comment, tracing to the board-path
"Resolved decision 2" single-trust-domain ruling — the posture lives in the
implementing comment, not the DL-200 row text) without superseding DL-200 (the
ForgeCaller seam shape stands); edits no existing row.

## Open Questions

- **OQ-1 (load-bearing, DEFERRED TO MATT): enforcement default — fail open
  or fail closed?** §A4 drafts `EnforceScopes` default **false**, so on a
  Beta deployment a misconfiguration (the flag simply unset) fails OPEN:
  scope enforcement silently off on the exact tier where Matt ruled it
  mandatory. (a) Keep default-false: zero behavior change
  for every existing deployment (the all-optional posture the `ForgeConfig`
  doc comment records in `go/server/serve.go`), but Beta safety hangs on one
  remembered
  config bit. (b) Default fail-CLOSED with an explicit
  `DisableScopeEnforcement` Dogfood opt-out: Beta-safe by default, but every
  existing deployment must set the opt-out at upgrade or every forge write
  starts rejecting. **Author's lean, explicitly NOT a decision:** (b) — a
  security control whose zero value means "off" invites exactly the
  silent-open misconfig the ruling exists to prevent, and the cost is one
  config line per Dogfood deployment versus a silent authz hole on Beta.
  Matt rules at freeze; this record does not resolve it.
- **OQ-2 (load-bearing): grant surface for Beta operators.** MVP ships the
  declarative config seed + store methods only — no public RPC. Is that
  enough for Beta, or does Beta need a `GrantForgeScope` admin RPC/tool at
  launch? **Recommendation:** config-seed-only for this slice (no-human-clicks
  is satisfied by config-as-code; an RPC is additive later); file the RPC as a
  follow-up issue.
- **OQ-3 (load-bearing): grant granularity.** Designed: grants attach to the
  owning user and cover the whole fleet (agent-or-owner check), per the
  standing "an agent acts within its owner's space" ruling
  (the `requireGroupCreateAuthz` doc comment in
  `go/internal/store/authz.go`); schema admits per-agent rows
  additively. Confirm Matt wants owner-level MVP rather than
  per-agent-required. **Recommendation:** owner-level MVP.
- **OQ-4 (non-load-bearing): read arms.** Reads stay ungated this slice
  (none of `getIssue`/`getPullRequest`/`listIssues` in
  `go/server/forge.go` takes a caller parameter, and the
  ruling targets writes). **Recommendation:** accept; file a follow-up for
  read-side scope parity when tracked-read privacy matters (multi-tenant).
- **OQ-5 (non-load-bearing): wildcard grammar.** `repo = '*'` grants a whole
  coordinate; no owner-prefix wildcards (`owner/*`) in MVP.
  **Recommendation:** accept — prefix wildcards are additive
  (`repo LIKE` variant) and unneeded at Beta's grant volume.
- **OQ-6 (load-bearing, DEFERRED TO MATT): git-op scope mechanism, the PAT
  fallback, and #634 sequencing.** §A5 designs the git clone/push/pull leg
  as **credential-narrowing**: when GitHub App config is present (RIG-2732
  / #634 — this leg DEPENDS on that record's App landing), the container
  credential is an installation token minted narrowed (the `repositories`
  body field, verified against the GitHub REST docs — the A5 load-bearing
  premise) to the workstream repo plus the account's `account_forge_scopes`
  repo set (the §A5 self-clone invariant). Three forks for freeze:
  (i) **mechanism** — (a) credential-narrowing (recommended: zero
  in-container enforcement code, GitHub rejects natively, nothing the agent
  can tamper with from inside), vs (b) a custom in-container credential
  helper, vs (c) a server-side git proxy — (b) and (c) rejected in
  Alternatives as strictly heavier for the same result.
  (ii) **PAT fallback** — when NO App is configured, per-account mint-time
  narrowing is unavailable (a classic PAT is not repo-narrowable at all; a
  fine-grained PAT is narrowable only statically at creation, §A5), so
  PAT-only git-op scope is (a) unenforced with a loud startup Warn (the
  design's drafted posture, §A4 — whether the Warn should be a hard startup
  fail instead is part of this fork), (b) mechanism (b)/(c) after all, or
  (c) a fine-grained PAT statically repo-scoped at creation to the
  deployment's granted-repo union — coarser than per-account installation
  tokens but enforceable without an App, partially honoring "unconditional"
  for a PAT-only Beta. **Recommendation:** (a)-with-loud-Warn; (c) is the
  honest middle if Matt wants a PAT-only Beta bounded.
  (iii) **sequencing** — does #601 freeze WHOLE, or do A1-A4 freeze now
  with A5/T4/T4.5/T5 CONTINGENT on #634's App landing (re-ratified if
  #634's App shape moves)? T1-T3 have zero dependency on #634.
  **Recommendation:** the contingent split — freeze A1-A4 unconditionally,
  mark A5/T4/T4.5/T5 contingent on #634, and do not hold T1-T3 behind an
  App that has not landed. (Folding the git-op leg into this record was
  Matt-directed; the split is sequencing only, never a record split.)
  **Designed against:** credential-narrowing when App present; PAT-only
  git-op scope unenforced-but-loud. Matt rules at freeze; this record does
  not resolve it.
- **OQ-7 (load-bearing, DEFERRED TO MATT): is the git-op allowlist the
  write set, or a distinct read set?** The narrowed credential is minted
  from `account_forge_scopes` — a WRITE allowlist — but a git credential
  also gates CLONE (read), and the §A5 self-clone invariant already forces
  one read-shaped exception (the workstream repo is always clonable). Fork:
  (a) the git-op scope is exactly the write set with the workstream repo
  implicitly granted for read/clone — one column, one table, no new grant
  surface; or (b) a distinct READ set that is a superset of the write set —
  a second column or separate read-grant rows, letting an agent clone repos
  it may not write. **The regression to weigh:** narrowing clone to
  {workstream repo + write set} removes what the host-wide token allows
  today — cloning a read-only dependency repo (a sibling library, a
  reference repo) the agent will never write — so (a) trades that
  multi-repo-read capability for the scoping. **Recommendation:** (a) —
  workstream-repo-implicitly-in-scope plus the write set governing
  everything else; no separate read column at MVP, additive later (a read
  column widens the schema without breaking (a)'s semantics) — accepting
  the multi-repo-clone loss as a Beta tradeoff. If "self-clone whatever it
  needs" must be preserved, (b) is the path. Note the honest asymmetry with
  OQ-4: forge-API reads stay ungated this slice while git clone is scoped —
  because the git credential is a single token gating both directions.
- **OQ-8 (load-bearing, DEFERRED TO MATT): where does the workstream-repo
  input come from?** The A5 self-clone invariant, the T4 mint's
  `workstreamRepo` argument, and OQ-7 all need the agent's own spawn-target
  repo as a server-side input at mint time — and it is not one. RIG-1527
  (Matt, 2026-07-29) deliberately removed repo carriage from provision:
  `ProvisionAgentWorkspaceRequest` carries no repo ("the agent self-clones
  whatever it needs after launch" — the repo-carriage-removed comment inside
  that message in `proto/compass/v1/compass.proto`),
  `agent_accounts` has no repo column (its `CREATE TABLE` in
  `go/internal/store/migrations/0001_init.sql`), and neither
  `StartAgentSessionRequest` nor `SpawnAgentRequest` carries one. The git-op
  leg therefore requires RE-INTRODUCING a per-agent/per-provision
  workstream-repo association — via a new provision/spawn field, an
  `agent_accounts` column, or a store-side spawn-target record — which
  reverses RIG-1527. **This is the blocking precondition for T4/T4.5/T5**
  (they cannot be built without the input) and it reverses a prior Matt
  ruling, so it is Matt's call, not the author's. **Recommendation:** the
  narrowest reversal — a store-side spawn-target record keyed by
  `container_name` written at provision, read by the T4.5 refresh-scheduler
  registry — rather than re-adding a wire field, since only the server needs
  it and the Runner still self-clones. Matt rules at freeze.

The draft's former rejection-text question (whether the refusal message may
differ from existing not_found texts) is resolved in-design, not open: the
out-of-scope refusal is byte-identical to the provider-403/404 flatten text
`"forge: artifact not found"` — the `case 403, 404:` arm of `mapForgeError`
in `go/server/forge.go` — see §A3.
