# Design addendum: Service subject principal (`SubjectService = 2`)

Status: Active
Tracking: RIG-2863 / RIG-3122 (parent RIG-1715, RIG-2845)

Ledger: DL-327 (this PR), Topology & tiers. Amends the two-kind token-subject
seal frozen by the retired v0.6 milestone record (see Problem / Intent); no
rows superseded. Per the corpus convention, the frozen record is amended by
this NEW record, never rewritten.

## Problem / Intent

The token-subject model is sealed to exactly two principal classes. The seal
lives in code, citing its frozen record:

- `go/internal/store/types.go:90-91` — "Sealed to exactly these two
  (design.md: 1175-1183)." with the two consts at `types.go:94-99`:
  `SubjectAccount SubjectKind = 0` / `SubjectRunner SubjectKind = 1`.
- `go/internal/store/migrations/0001_init.sql:377` —
  `subject_kind SMALLINT NOT NULL CHECK (subject_kind IN (0, 1))`, under the
  header comment (`0001_init.sql:371-373`): "Subject-typed token store
  (design.md:1175-1183) … subject_kind is 0 account / 1 runner".
- `go/internal/store/tokens.go:20` —
  `SubjectKind: int16(subj.Kind), //nolint:gosec // G115: SubjectKind is a
  CHECK-constrained 0/1 enum (tokens.subject_kind), always within int16`.

The `design.md:1175-1183` those comments cite is the RETIRED v0.6 milestone
record, `docs/designs/product/compass-0.6/design.md` — deleted in commit
`2ccb7b2e` ("refactor(design): retire the v0.3–v0.8 milestone records,
consolidate to an architecture-lineage record (RIG-2453)"). Verified against
`git show 2ccb7b2e~1:docs/designs/product/compass-0.6/design.md`, lines
1177-1185: "Tokens (subject-typed, so a Runner subject and an account subject
share the store but never collide — the OQ7 prefix-separation T4 depends on):
… `SubjectKind ∈ {Account, Runner}` … (returns the subject *with its kind*, so
a door can reject a cross-kind token …)". That record's decision authority now
lives in the ledger (the DL-012 bearer-token-door lineage,
`meta/compass-architecture-lineage/design.md` §"The authenticated door");
**this addendum is the amendment surface for the seal**.

The seal blocks a first-party principal class the system now needs: the RIG-1715
LLM gateway (`docs/designs/server/compass-server-llm-gateway/design.md`) is a
supervised compute tier that authenticates BACK to the Server — its
"RPC-to-Server-for-creds" surface is "a narrow, stack-token-authenticated
Server surface (list credentials for pool / write back refreshed OAuth tokens /
CAS disable)" (llm-gateway record, §Approach, lines 348-351). That stack token
is neither an account subject nor a Runner subject; a future MCP gateway
presents the same problem. Intent: admit a THIRD token-subject principal class,
`SubjectService SubjectKind = 2`, for every first-party supervised compute tier
that authenticates back to the Server.

## Approach

**Matt-ruled (frozen; this record captures the ruling, it does not reopen
it):** `SubjectService = 2` is ONE principal CLASS for all supervised compute
tiers — the LLM gateway now, an MCP gateway later — NOT a kind per tier.

- **`SubjectKind` separates principal CLASSES, not instances.** Each supervised
  tier presents its OWN minted service token with a DISTINCT Subject ID (e.g.
  `llm-gateway` vs `mcp-gateway`). Tiers are isolated from each other by
  Subject ID + per-surface authorization, not by minting a new kind per tier.
  The precedent is the account door: `SubjectAccount` is one class covering
  users AND agents, with per-surface authz layered on the resolved identity —
  e.g. the owner resolution over `agent_accounts.owner_user_id`
  (`go/internal/comms/comms.go:114-115`: "an agent caller resolves to its
  owner_user_id, a user caller to itself") discriminating what a given account
  subject may do — layered ON TOP of the tenant GUC the request path arms
  (`tenant_tx.go:135-143`), which is where the account door's isolation
  substantially comes from, not `owner_user_id` alone. A service door
  authorizes the resolved `Subject.ID` against the surface's allowlist the same
  way; its TENANT posture is not settled by this record — see OQ-4.
- **The existing cross-door kind-gate isolates the new class for free.**
  `ResolveToken` (`go/internal/auth/token.go:102-116`) is the ONE shared
  resolver: `if subj.Kind != want { return store.Subject{}, ErrWrongKind }`
  (`token.go:112-114`). Its doc states the door contract (`token.go:98-101`):
  "Both the account door (want=SubjectAccount) and the Runner door
  (want=SubjectRunner) share this one resolver, so the security-critical
  resolve+kind-gate lives and is tested in exactly one place; each door adds
  only its own trivial typed wrap on the returned Subject." A service door is
  one more trivial typed wrap passing `want=store.SubjectService` — an account
  or Runner token presented at the service door (and a service token at the
  account/runner doors) auto-rejects as `ErrWrongKind`, exactly as
  `SubjectAccount` and `SubjectRunner` isolate today (existing wraps:
  `go/internal/runnerhub/auth.go:79` `b.resolve(ctx, token,
  store.SubjectRunner)`; the account-bearer interceptor in
  `go/internal/auth/interceptor.go`). No new resolver.
- **Named `SubjectService`** — NOT `SubjectStack` (reads as the deployment
  stack), NOT `SubjectGateway` (narrower than the class).

Shipping this addendum BEFORE the implementation (PR2 — the enum-half
implementation PR, T1/T2/T3/T5, throughout this record) is deliberate: the enum
NUMBER and the `tokens.subject_kind` CHECK constraint land in `0001_init.sql`
and are painful to change once any non-disposable database has APPLIED v1 (the
condition is version-keyed, not row-count-keyed — see Global Constraints).

## Global Constraints

- The two existing kinds keep their numbers: `SubjectAccount = 0`,
  `SubjectRunner = 1` (`types.go:96-98`). `SubjectService = 2` is additive;
  numbers are append-only, never reused.
- The one-resolver invariant holds over DOORS: every door authenticates
  through `auth.ResolveToken` (`token.go:102`); no door grows its own resolve
  or kind-check. One non-door path is deliberately outside it:
  `runnerhub.RunnerTokenRegistered` (`mint.go:80-93`) is a KIND-AGNOSTIC
  store-level existence check — it resolves a hash and returns true for ANY
  resolving subject, never comparing `Kind` — used by the runner-credential
  provisioning heal paths, not a door — its two callers are
  `go/cmd/compass-mint-runner-token/main.go:157` (the operator CLI) and
  `go/internal/stack/adapters/token.go:87` (the AUTOMATED stack-boot heal,
  not operator-driven). The third class widens its
  false-"registered" surface by one: a `SubjectService` token hash that
  appeared in a runner's token file would report registered, so the heal path
  would keep it instead of rotating and the runner would then fail the kind
  gate at `runnerhub/auth.go:79`. Not an escalation (the door still fails
  closed); reaching it via the CLI leg needs an operator pasting a service token
  into runner state, while the `token.go:87` heal leg reaches it only from a
  token already in a runner's own resolved state — so neither is an untrusted
  input path. A non-load-bearing follow-up for the T4/issuance slice could
  compare the resolved `Kind` before treating a token as registered, landing in
  exactly those two callsites.
- The token-existence-oracle posture holds: every door maps
  `ErrTokenNotFound` / `ErrTokenRevoked` / `ErrWrongKind` to the same bare
  `CodeUnauthenticated` (`token.go:92-97`).
- `0001_init.sql` is edited in place (the CHECK widen lands there, not in a
  new migration — the reason this record precedes PR2). This is safe ONLY
  while every environment is disposable: `migrate()` skips any version already
  recorded in `schema_migrations` (`store.go:157-164`), and the refuse-to-serve
  guard compares max-embedded vs recorded version (both `1`, `store.go:169-176`),
  so a database that has ALREADY applied v1 keeps the old
  `CHECK (subject_kind IN (0, 1))` and boots clean past the guard — token-table
  emptiness is irrelevant, the constraint is materialized in that database's
  catalog at v1-apply time regardless of row count, and a later SubjectService
  insert then fails the stale CHECK at runtime. So every existing dev/CI/
  self-host database must be wiped and re-migrated, or the change moves to a
  new `NNNN_*.sql ALTER`. This is the established pre-GA in-place-edit posture
  (RIG-3106 #830 RLS enforcement, RIG-2861 T1 #715 tenant schema; the pgtest
  harness resets per run).

## Plan

PR2 executor contract — TWO scopes, each grounded on the current line. **The
enum half (T1/T2/T3/T5) lands NOW as PR2** — every file+line it names exists
today, and it is the urgent half (the enum number + `tokens.subject_kind`
CHECK are painful to change once any non-disposable database has applied v1,
per Global Constraints). **The
door half (T4) lands WITH the service surface**, which does not exist in the
tree yet: it is delivered by RIG-2863 (RIG-1715 T2 — the AuthStorage-over-compass
adapter + the LLM gateway's stack-token RPC surface, currently Backlog), and
T4 mounts on it there, ordered AFTER that surface. SubjectService token
ISSUANCE (the mint path, no corpus task owns it yet) is OQ-3 below.

### T1 — `SubjectService` const + seal-comment update

`go/internal/store/types.go`: add the third const to the block at
`types.go:94-99` and update the seal SENTENCE ("Sealed to exactly these two
(design.md: 1175-1183).", spanning `types.go:90-91`) to name three kinds and
cite THIS record. Edit it SENTENCE-scoped, not by wiping the `:90-91` line
range: line 90 also carries the TAIL of the preceding cross-door example clause
("account token on RunnerService).", the clause T4 later refreshes), so a
wholesale line-range replace would truncate that clause. Rewrite only the seal
sentence in place.
Also extend the `Subject.ID` doc (`types.go:106-107`: "ID is the
AccountID (SubjectAccount) or the Runner id (SubjectRunner)…") to name the
service id space (a stable service name, e.g. `llm-gateway`).
Also refresh the `Subject` struct doc at `types.go:101-103` (the two-kind
enumeration — "the id of the account or Runner it authenticates") in the same
comment block: it is a KIND-axis enumeration that goes stale the moment the
third kind lands (three kinds exist after T1), so it belongs in T1, not the T4
door-refresh list — leaving it would put a "the account or Runner" struct doc
directly above an `ID` field doc T1 has just extended to a third id space.

Interfaces:

```go
// produces (append to the existing const block, types.go:94-99):
// SubjectService is a first-party supervised compute tier (LLM gateway,
// future MCP gateway) authenticating back to the Server. One class for all
// tiers; instances are distinguished by Subject.ID, isolated per-surface.
SubjectService SubjectKind = 2
// seal comment becomes: "Sealed to exactly these three
// (docs/designs/server/compass-service-subject-principal.md)."
```

### T2 — CHECK constraint admits 2

`go/internal/store/migrations/0001_init.sql:377`: the column line changes and
the table header comment at `0001_init.sql:372-373` ("subject_kind is 0 account
/ 1 runner") gains `/ 2 service`.

Interfaces:

```sql
-- consumes: CHECK (subject_kind IN (0, 1))   -- 0001_init.sql:377
-- produces: CHECK (subject_kind IN (0, 1, 2))
```

After the edit, run `moon run compass-go:sqlc-gen` and confirm no drift (the
checked-in `internal/store/db` tree is the source of truth; the `sqlc-drift`
gate fails closed on any stale byte — a CHECK-only widen regenerates
identically, but confirm), and expect `sql-migration-gate` (squawk + sqruff
over `go/internal/store/migrations/*.sql`) to re-run over the edited file.

### T3 — nolint text tracks the enum

`go/internal/store/tokens.go:20`: the gosec waiver's justification names the
constrained set; it must not go stale.

Interfaces:

```go
// consumes: //nolint:gosec // G115: SubjectKind is a CHECK-constrained 0/1 enum (tokens.subject_kind), always within int16
// produces: //nolint:gosec // G115: SubjectKind is a CHECK-constrained 0/1/2 enum (tokens.subject_kind), always within int16
```

### T4 — service-door mount (lands WITH the RIG-2863 service surface, NOT in PR2)

The service surface (first consumer: the LLM gateway's stack-token RPC surface
per the llm-gateway record) authenticates via the shared resolver with the new
want — a typed wrap in the pattern of `runnerhub`'s
(`go/internal/runnerhub/auth.go:79`: `b.resolve(ctx, token,
store.SubjectRunner)`), never a second resolver.

Interfaces:

```go
// consumes: auth.ResolveToken(ctx, st, presented, want store.SubjectKind) (store.Subject, error)  // token.go:102
// produces: the service door's bearer authenticate calling
subj, err := auth.ResolveToken(ctx, st, presented, store.SubjectService)
// all three sentinels map to bare CodeUnauthenticated (token.go:92-97);
// per-surface authz then checks subj.ID against the surface's service allowlist.
```

Doc refresh (T4 owns it, since the third door wrap is where the subject-KIND
prose goes stale): this record changes the subject-KIND axis (account/Runner
grows a third kind), NOT the door count. The door count is already three at
`eb5ef7a1` — the `compass.v1` surface has three doors (`serve.go:572`,
`:647`, `:662`: shipped Unix socket, optional dev loopback, optional
authenticated network) plus the Kind-gated RunnerService mount
(`network_door.go:313` behind `runnerhub/auth.go:79`'s `SubjectRunner`
bearer) — so a "two doors" claim is NOT made stale by this record's third kind.
The two-door COUNT sentences that remain (`auth/doc.go:7`, and the
`both doors` sites at `interceptor.go:86`/`:129`, `service.go:539`/`:604`)
are scoped to the `auth` PACKAGE's OWN pair — network + socket, the two doors
whose handlers read the caller via `CallerFrom` (`auth/doc.go:2` scopes the
package to "the network door"; RunnerService authenticates in `runnerhub`, not
`auth`) — so they are narrow-and-true within that scope, independently of
SubjectService; they are OUT OF SCOPE for both PR2 and T4. The CONTRACT is a
discovery rule for the subject-KIND sites only (the line-pinned list below is
evidence, not the boundary): refresh every NON-GENERATED comment under `go/`
and `proto/` that ENUMERATES the account/Runner subject KINDS — matching
(case-INSENSITIVELY) roughly `/cross-door|cross-kind|account (token|subject|
door)|Runner (token|subject|door)|(the )?other door|two mandatory/` — since any
two-KIND subject enumeration goes stale when the third kind lands. Exclude
`go/gen/**` and `go/internal/gen/**` (regenerate those from the proto instead,
never hand-edit — see T4's proto note below); test prose IS in scope. The
door-COUNT axis is deliberately NOT swept here: it is orthogonal to the kind
axis this record adds, and the `auth`-scoped count sentences above are correct
within their package scope. The subject-KIND sites known at authoring time (verified at `eb5ef7a1` — these
are the T4 refresh targets):
`go/internal/auth/token.go:98-99` (the shared-resolver door enumeration — "Both
the account door … and the Runner door … share this one resolver"),
`token.go:79-81` (`ErrWrongKind`'s account-vs-Runner examples),
`go/internal/auth/interceptor.go:140-141` (the cross-door failure enumeration),
the `go/internal/runnerhub/auth.go:4-14` package doc (the cross-door-rejection
framing), `go/internal/store/types.go:89-90` (the
cross-door EXAMPLE clause — "reject a cross-kind token (a Runner token on
CompassService/CommsService, an account token on RunnerService)" — which SHARES
line 90 with the "Sealed to exactly these two" seal sentence T1 rewrites, so
T1 must edit that seal sentence IN PLACE, sentence-scoped not line-range-scoped,
preserving the leading "account token on RunnerService)." on line 90),
`types.go:86-88` (the `SubjectKind` doc opener — "a Runner subject and an
account subject share the token store but never collide"),
`go/internal/runnerhub/mint.go:5-7` ("a Runner subject and an account subject
share one store but can never collide"),
`go/internal/runnerhub/handler.go:68-69` ("an account token never reaches here
… the RunnerService cross-door rejection") and `:260-261` ("an account token is
Unauthenticated here, the OQ7 cross-door rule"),
`go/server/network_door.go:229-231` ("an account token is Unauthenticated
there, and a Runner token is Unauthenticated on the account/comms doors: the
OQ7 cross-door rule") and `:299-301` ("an account token is Unauthenticated here
and a Runner token is Unauthenticated on the CompassService/CommsService doors
above (OQ7 cross-door rejection)"), and — the sharpest, because it states a
literal COUNT of cross-door rejection tests that grows when a third KIND lands —
`proto/compass/v1/runner.proto:53-56` ("the RunnerService side of the TWO
mandatory cross-door rejection tests"); its sibling `:183-184`
("account-subject tokens rejected") is an ordinary stale two-kind enumeration,
not a count. Editing `runner.proto:53-56` forces a regenerate: that prose is
mirrored verbatim into the checked-in generated tree
(`go/internal/gen/compass/v1/compassv1internalconnect/runner.connect.go:98`,
`:367`), which is NOT gitignored and is drift-gated — `compass-proto:drift`
(in `proto:ci`) fails closed on any byte diff — so regenerate and commit that
tree in the same slice, never hand-edit it. `network_door.go` lives at
`go/server/`, NOT `go/internal/` like the rest — the one cited file outside
`go/internal/`.

The door-COUNT sites are deliberately OUT of scope (this record changes the kind
axis, not the door count): `go/internal/auth/doc.go:7-19` ("Two doors reach the
same compass.v1 service"), `interceptor.go:86` ("both doors") and `:129`
("both doors reject identically"), `go/server/service.go:539`/`:604` ("attach
one on both doors"). All are scoped to the `auth` package's OWN network+socket
pair (the `CallerFrom` doors) and are narrow-and-true within that scope — a
third KIND does not falsify them, and the third door (RunnerService) already
exists at `eb5ef7a1`, so they are neither PR2 nor T4 work.

### T5 — cross-door pgtest

Extend the existing cross-door cases in `go/internal/auth/token_test.go:134-154`
(auth harness `go/internal/auth/harness_pgtest_test.go`, build tag
`pgtest && unix`) with the three-kind cross-door matrix, and the store
round-trip in `go/internal/store/tokens_test.go` (harness
`go/internal/store/harness_test.go`, build tag `pgtest`). The matrix: a
`SubjectService` token resolves at `want=SubjectService`; presented at
`want=SubjectAccount` and `want=SubjectRunner` it fails `ErrWrongKind`; an
account token and a Runner token presented at `want=SubjectService` each fail
`ErrWrongKind`. Also two store-level round-trips: `PutTokenHash` with
`Subject{Kind: SubjectService, ID: "llm-gateway"}` persists (proving the T2
CHECK admits 2) and `ResolveTokenHash` returns the kind intact; and
`PutTokenHash` with `Subject{Kind: SubjectKind(3), ID: "nope"}` FAILS with a
constraint violation (proving the widened CHECK is still a closed set of
exactly `{0, 1, 2}` — not dropped or over-widened to admit 3). Assert only
`err != nil` on that call — the ID is non-empty and the hash fresh, so the
widened CHECK (SQLSTATE 23514) is the sole possible failure source. Do NOT add
a new store sentinel or SQLSTATE constant for it: the store maps only 23505 →
`ErrConflict` and 23503 → `ErrInvalidArgument` (`errors.go:9-12`), and a 23514
falls through to the bare wrap at `tokens.go:26` — introducing a typed sentinel
is a store API change this record does not scope (T2/T3 touch only the CHECK
and the nolint text).

Interfaces:

```go
// consumes: auth.ErrWrongKind (token.go:82), store.PutTokenHash (tokens.go:14),
//           store.ResolveTokenHash (tokens.go:36), pgtest harness
// produces: pgtest cases in the existing auth/store pgtest files; no new harness.
```

## Tasks

- [ ] T1: `SubjectService SubjectKind = 2` + seal comment (`types.go:90-99`) + `Subject.ID` doc
- [ ] T2: `0001_init.sql:377` CHECK `IN (0, 1)` → `IN (0, 1, 2)` + header comment `:372-373`
- [ ] T3: `tokens.go:20` nolint `0/1` → `0/1/2`
- [ ] T4 (lands in RIG-2863 = RIG-1715 T2, NOT PR2): service-door mount via `ResolveToken(..., store.SubjectService)` + per-surface Subject-ID authz, mounted on the stack-token RPC surface that slice delivers
- [ ] T5: cross-door pgtest matrix (3×3 kind-gate + CHECK-admits-2 round-trip)

## Open Questions

- **CHECK shape: inline IN-list vs lookup table (non-load-bearing for THIS
  slice, surfaced for the record).** The current constraint is an inline
  `CHECK (subject_kind IN (0, 1))` (`0001_init.sql:377`); this record extends
  it in place. A `subject_kinds` lookup table with an FK would make future kind
  additions a row-insert instead of a constraint edit — but every kind addition
  is a Matt-ruled design event anyway (this record exists precisely because
  one is), so the schema ceremony buys nothing over the one-line CHECK edit.
  Recommendation: keep the inline IN-list. Only escalate if Matt expects
  kind churn beyond design-gated additions.
- **OQ-3 (deferral naming the owning slice — NON-load-bearing for PR2, the enum
  half): SubjectService token ISSUANCE lands in the RIG-2863 slice (RIG-1715 T2),
  alongside this record's T4.** Both
  existing kinds have a real mint path — `IssueAccountToken` (`token.go:51-55`)
  for `SubjectAccount`, `runnerhub.MintRunnerToken` (`mint.go:103`) + the
  `compass-mint-runner-token` CLI for `SubjectRunner`. No corpus task mints a
  `SubjectService` token yet (this record's T5 only writes rows test-side via
  `PutTokenHash`), so the enum + door would otherwise ship with no principal able
  to pass the door. Resolution (driver call, boring-consistent — mirrors the
  existing mint paths, no design fork): issuance lands in the RIG-2863 (RIG-1715
  T2) slice ALONGSIDE the service surface T4 mounts on — an `IssueServiceToken`
  (boot/store fn or an operator CLI, mirroring `MintRunnerToken`), minting under
  a distinct Subject ID per tier (`llm-gateway`, later `mcp-gateway`). PR2 (the
  enum half, T1/T2/T3/T5) does NOT depend on it; it is recorded here so the T4
  slice owns it explicitly rather than an executor improvising a mint path on a
  security-critical door.
- **OQ-4 (service-door tenant posture — load-bearing for T4, NON-load-bearing
  for PR2). The record fixes the auth PRINCIPAL but not its TENANT posture; T4
  must not improvise one.** A service door is a request path, and under RLS a
  request-path store statement arms `SET LOCAL ROLE compass_app` plus a tenant
  GUC (`tenant_tx.go:135-143`), with `resolveTenant` falling back to the
  BOOTSTRAP tenant when the context carries none (`tenant.go:54-59`). So a
  service-door RPC that resolves a `SubjectService` token but sets no tenant
  runs bootstrap-scoped and sees only that tenant's rows — which silently
  breaks this record's own first consumer: the LLM gateway's stack-token
  surface must serve EVERY tenant's provider credentials, isolating them by
  per-tenant pool scoping enforced server-side, not by the process (a
  compromised gateway holding one stack token can read every tenant's creds —
  `docs/designs/server/compass-server-llm-gateway/design.md:333-337`; pools
  resolve from `owner_user_id`, :377-378). The only cross-tenant escape is
  `WithSystemRole` (BYPASSRLS), and it is explicitly fenced from request paths:
  "applied ONLY at the four named background-loop entrypoints … a request-path
  call NEVER sets it" (`tenant_tx.go:41-47`; role scope :19-22). A service door
  is a request path, so this record does NOT authorize it to take that escape.
  The three shapes T4 must choose among (the executor may NOT improvise):
  (a) the door resolves a tenant per request from a request-carried selector,
  validated against the Subject.ID allowlist, and calls `store.WithTenant` —
  request path stays tenant-scoped and fail-closed; (b) the surface is
  deliberately cross-tenant, which requires a NEW, explicitly Matt-ruled
  widening of the `WithSystemRole` background-loop exemption
  (`tenant_tx.go:41-47`) to a request-path door — a security-boundary change
  this record does NOT grant; or (c) tenancy is formally deferred to the
  RIG-2863 (RIG-1715 T2) slice as a BLOCKING prerequisite of T4, so the surface
  cannot ship without a ruling. Resolution: shape chosen with the T4 surface in
  the RIG-2863 slice; PR2 (the enum half, T1/T2/T3/T5) does NOT depend on it —
  the enum and CHECK carry no tenant posture. Recorded here (not left implicit)
  so the T4 executor is handed a named fork, not an undesigned security choice.
- Non-load-bearing deferral: the canonical Subject-ID registry for service
  principals (e.g. `llm-gateway`, `mcp-gateway` as named constants vs
  config-supplied strings) is an implementation detail of the T4 surface's
  allowlist, and lands WITH T4 in the RIG-2863 slice (RIG-1715 T2) — not in
  PR2. It does not affect the schema or the enum, so PR2 (T1/T2/T3/T5) does
  not touch it.
