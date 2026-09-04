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
  subject may do. A service door authorizes the resolved `Subject.ID` against
  the surface's allowlist the same way.
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

Shipping this addendum BEFORE the implementation (PR2) is deliberate: the enum
NUMBER and the `tokens.subject_kind` CHECK constraint land in `0001_init.sql`
and are painful to rename once token rows exist.

## Global Constraints

- The two existing kinds keep their numbers: `SubjectAccount = 0`,
  `SubjectRunner = 1` (`types.go:96-98`). `SubjectService = 2` is additive;
  numbers are append-only, never reused.
- The one-resolver invariant holds: every door authenticates through
  `auth.ResolveToken` (`token.go:102`); no door grows its own resolve or
  kind-check.
- The token-existence-oracle posture holds: every door maps
  `ErrTokenNotFound` / `ErrTokenRevoked` / `ErrWrongKind` to the same bare
  `CodeUnauthenticated` (`token.go:92-97`).
- `0001_init.sql` is the initial migration — editable only while it remains
  unshipped-to-data; the CHECK edit lands there, not in a new migration
  (the reason this record precedes PR2).

## Plan

PR2 executor contract — TWO scopes, each grounded on the current line. **The
enum half (T1/T2/T3/T5) lands NOW as PR2** — every file+line it names exists
today, and it is the urgent half (the enum number + `tokens.subject_kind`
CHECK are painful to change once token rows exist, per Problem / Intent). **The
door half (T4) lands WITH the service surface**, which does not exist in the
tree yet: it is delivered by RIG-2863 (RIG-1715 T2 — the AuthStorage-over-compass
adapter + the LLM gateway's stack-token RPC surface, currently Backlog), and
T4 mounts on it there, ordered AFTER that surface. SubjectService token
ISSUANCE (the mint path, no corpus task owns it yet) is OQ-3 below.

### T1 — `SubjectService` const + seal-comment update

`go/internal/store/types.go`: add the third const to the block at
`types.go:94-99` and update the seal sentence at `types.go:90-91` ("Sealed to
exactly these two (design.md: 1175-1183).") to name three kinds and cite THIS
record. Also extend the `Subject.ID` doc (`types.go:106-107`: "ID is the
AccountID (SubjectAccount) or the Runner id (SubjectRunner)…") to name the
service id space (a stable service name, e.g. `llm-gateway`).

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

### T5 — cross-door pgtest

Extend the store/auth pgtest coverage (harness per
`go/internal/store/harness_test.go`) with the three-kind cross-door matrix: a
`SubjectService` token resolves at `want=SubjectService`; presented at
`want=SubjectAccount` and `want=SubjectRunner` it fails `ErrWrongKind`; an
account token and a Runner token presented at `want=SubjectService` each fail
`ErrWrongKind`. Also a store-level round-trip: `PutTokenHash` with
`Subject{Kind: SubjectService, ID: "llm-gateway"}` persists (proving the T2
CHECK admits 2) and `ResolveTokenHash` returns the kind intact.

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
- [ ] T4 (RIG-2863 T2, NOT PR2): service-door mount via `ResolveToken(..., store.SubjectService)` + per-surface Subject-ID authz, mounted on the stack-token RPC surface that slice delivers
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
  half): SubjectService token ISSUANCE lives in the RIG-2863 T4 slice.** Both
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
- Non-load-bearing deferral: the canonical Subject-ID registry for service
  principals (e.g. `llm-gateway`, `mcp-gateway` as named constants vs
  config-supplied strings) is a PR2 implementation detail of the T4 surface's
  allowlist; it does not affect the schema or the enum.
