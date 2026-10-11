# Compass tracker config contract (RIG-5077)

## Problem / Intent

Tracker: RIG-5077. Settings lost its Tracker section in DL-432. That section
edited the client `TrackerConfig` in `apps/ui/src/stub-data.ts`
(`kind`, `handle`, `mapping`), but the value reached only the fixture seam.
`createFixtureTrackerSeam` in `apps/ui/src/tracker.ts` ignores it:

```ts
export function createFixtureTrackerSeam(
	_config: TrackerConfig = DEFAULT_TRACKER_CONFIG,
): TrackerSeam {
```

The settings record (`../../ui/compass-settings/design.md`, Resolved
Question 1) says "a server-backed Tracker section returns with the tracker
contract". No contract exists. `proto/compass/v1/compass.proto` has no
tracker-config message or RPC.

The server cannot read tracker status today:

- `linearIssue.toIssue` in `go/internal/forge/linear.go` keeps only the
  open/closed truth and drops the state name:
  `State: mapLinearState(r.State.Type),`.
- The `issues` table admits no Linear row. `CREATE TABLE issues` in
  `go/internal/store/migrations/0001_init.sql` has
  `CHECK (forge_provider IN (1, 2, 3))`.
- `SourceTracker` in `go/server/board.go` has no producer. `board_test.go`
  keeps it referenced with `var _ = []SourceKind{SourceTracker, SourceAuto}`.

DL-129 says users set state in the tracker and Compass ingests the native
status through a reverse mapping. This record defines that mapping, where it
is stored, the function that applies it, the preconditions its caller must
honor, and the RPCs that read and write it. The Settings section (RIG-5008)
reads and writes through these RPCs.

Ledger-impact: none in this PR. Every fork below is unruled, and a decision
file's status must carry Matt's provenance. Decision rows are claimed with
`bun tools/dl-claim --ref RIG-5077` and written in this PR once Matt rules
the Open Questions, before the record freezes.

## Approach

### What the server can honor

Only the inbound status mapping. The other fields of the client
`TrackerConfig` have no server reader:

| Client field | Server today | In this contract |
| -- | -- | -- |
| `mapping.fromTracker` (status → state) | No reader. The Linear lane will ingest status (consumer below). | **Yes** |
| `mapping.toTracker` (state → status) | No writer. `newBoardService` leaves `mirror` nil ("no real forge-tracker write seam yet"). Agent transitions resolve by type in `defaultWorkflowState`, or by an explicit name. | No (OQ-2) |
| `kind` | Linear is the only tracker provider. Go has no Jira provider. | No (OQ-2) |
| `handle` | No server path lists a user's assigned issues. The Linear client authenticates as the app (`--forge-linear-client-id` in `go/cmd/compass-server/main.go`). | No (OQ-2) |

Linear credentials and the webhook secret stay deployment flags. They are
server secrets, not user settings.

### The resolve function

`tracker.ResolveLinearStatus` lives in a new leaf package,
`go/internal/tracker`. No existing package fits:

- `store` is persistence and should not learn Linear types.
- `ingest` "imports NO store" (`go/internal/ingest/ingest.go`).
- `forge` carries no Compass machinery ("those are canonical-only",
  `go/internal/forge/provider.go`).

The package imports `store` for `IssueState` and the config types, and
`forge` for the Linear type constants. It is pure:

- **In:** the Linear team key and the issue's workflow state (name, type).
- **Out:** an `IssueState` plus the raw status name for display, or
  `ok=false` for no transition.
- **No stored config:** the built-in default config applies, then the type
  default.

Rule order:

1. A rule whose `team_key` equals the team and whose `status_name` equals the
   name.
2. A rule with an empty `team_key` and the same name.
3. The type default.

| Linear `type` | Default `IssueState` |
| -- | -- |
| `triage`, `backlog` | `BACKLOG` |
| `unstarted` | `TODO` |
| `started` | `IN_PROGRESS` |
| `completed`, `canceled`, `duplicate` | `DONE` |
| any other value | none: `ok=false` (OQ-4) |

The seven types are the ones named in the doc comment on
`linearClosedStateTypes` in `go/internal/forge/linear.go` ("Verified against
Linear SDL WorkflowState.type"), and `TestMapLinearStateCoversEverySDLType`
pins them. T1 exports one list from `forge` so both mappers share it. The
closed three fold to `DONE`, as `mapLinearState` folds them to `closed`.

**Built-in default config.** One rule: `{team_key: "", status_name: "In
Review", state: IN_REVIEW}`. Linear has no review type, so "In Review" is a
`started` state. The UI's `LINEAR_STATUS_MAPPING` maps it to `in_review`.
Every other entry in that map already equals its type default. A tenant with
no stored row reads this config at version 0.

A rule may target only `BACKLOG` through `DONE`. `ARCHIVED` is excluded
because DL-129 sets it by the 24-hour auto-archive, and `WorkingIssueState`
in `apps/ui/src/stub-data.ts` already excludes it.

Rules match by name (OQ-5). Names are not unique on a team:
`resolveWorkflowState` rejects "%d workflow states named %q". Inbound mapping
is many-to-one, so two states with one name map to one state, which is safe.
A Linear rename moves no card, because the issue's state does not change. The
next move into the renamed state gets the type default.

### Consumer contract

compass #2078 (RIG-5059) is the consumer. Matt ruled its OQ-1 (c) on
RIG-5066: ingest Linear issues into `issues`. Its Linear ingest lane owns the
`issues` CHECK widen and the fetch, defines no mapping, and calls
`tracker.ResolveLinearStatus` once per issue. The call site is that lane's
board-ingest sink in `go/server`, after the forge-field upsert. A resolved
state goes to `boardService.SetIssueState` with
`TransitionSource{Kind: SourceTracker}`.

DL-129 suppresses echoes by comparing the observed status with
`toTracker(current state)`. This contract has no `toTracker` (OQ-1). The
consumer must honor these preconditions instead:

1. **Resolve only on a state change.** A webhook is a change when
   `linearStateChanged` in `go/internal/linearagent/data_event.go` is true
   (`updatedFrom.stateId` set). A sweep is a change when the observed state
   differs from the last one stored for the issue. A first observation is a
   change.
2. **Never un-archive on a closed move.** Skip a resolved `DONE` when the
   issue is `ARCHIVED`. `SetIssueState` compares only
   `current.State == target`, so it would otherwise commit `ARCHIVED → DONE`.
3. **Keep DL-129's recency guard.** Drop an observation older than the stored
   one.
4. **Skip `ok=false`.**
5. **Stay in the issue's tenant.** Load the config under the tenant the issue
   is upserted under, never under `WithSystemRole`. The store refuses that
   role (T3).

The consumer also owns persisting the raw status name and the last-observed
state. `store.Issue` has no tracker storage: "Prs and Tracker are NOT here in
this slice" (`go/internal/store/issues.go`). The existing display wire field
is `TrackerRef.status` in `compass.proto`.

The fetch must carry the name on both paths. The GraphQL fragment
`CompassIssueFields` in `go/internal/forge/linear.go` reads
`state { name type }`. The webhook's `dataState` in
`go/internal/linearagent/data_event.go` decodes only `type`.

An agent's explicit forge transition is a real Linear state change, and the
board follows it.

### Scope: one config per tenant

One Linear app credential serves the whole deployment. Every tenant sees the
same Linear states. But each tenant has its own `issues` rows and board
state, so the mapping is per tenant (OQ-3). The row is versioned like the
fleet `model_registry` singleton, with one row per tenant under RLS.

Today the Linear webhook and sweep set no tenant. `Store.resolveTenant` then
falls back to the bootstrap tenant. That is the only tenant production
creates: `BootstrapTenant` is the only non-test caller of `InsertTenant`.

A change applies at each issue's next state change. Stored states are not
rewritten (OQ-7).

### RPCs and authorization

There are two RPCs on `CompassService`, shaped like `GetModelRegistry` and
`PutModelRegistry` in `go/server/model_registry_service.go`:

- **Get** is open to any authenticated account. A tenant with no row gets
  version 0 and the built-in default config. It reports `can_edit`, so the
  UI can render read-only rows without a second call. `WhoAmIResponse`
  carries only `account_id`.
- **Put** replaces the whole config under compare-and-set on `int64
  expected_version`. A value of 0 seeds the first row. A stale version
  returns `store.ErrVersionConflict`, which maps to `CodeAborted`. A bad
  payload returns `ErrInvalidArgument`, which maps to `CodeInvalidArgument`.
  An empty rule list removes every rule, including the built-in one.

Put is `authenticatedOpen` in `classifyProcedure`, and the handler requires a
user account with `UserRoleAdmin` (OQ-6). The precedent is the tenant-scope
secret write in `resolveSecretScope` in `go/server/secrets_service.go`:

```go
if role != store.UserRoleAdmin {
	return 0, "", connect.NewError(connect.CodePermissionDenied, errors.New("tenant-scoped secret writes require an admin"))
}
```

`adminOnly` admits only the bootstrap admin (`AdminGate.check` in
`go/internal/auth/admin_gate.go`: `caller != g.admin`).

## Alternatives considered

- **Deployment singleton like `model_registry`.** Matches the credential's
  scope. OQ-3 (b).
- **Per-account mapping.** Two users would map one shared issue differently.
  Rejected.
- **Deployment flag.** No UI edit and a restart per change. Rejected: RIG-5008
  needs an RPC.
- **Rules keyed by workflow state id.** OQ-5 (b).
- **Resolver in `store`.** It would put Linear types in the persistence
  package, and its type list would fork from `forge`'s. Rejected.
- **Rows instead of JSONB.** Per-rule rows would need per-rule CAS. One
  versioned JSONB row matches `model_registry` and the whole-config Put.

## Plan

### Global Constraints

- **Public repo.** Never name private repositories, hosts, deployments or
  roadmap (`skill://compass-managed-boundary`). Tenant RLS is a core
  capability and may be described.
- **Additive proto.** Add new messages and two RPCs only. Change no existing
  field. Regenerate Go and TypeScript code.
- **Gate coverage.** Classify both procedures in `classifyProcedure`.
  `classify_exhaustive_test` reddens on an unclassified procedure.
- **Migrations.** Open migrations are folding into `0001_init.sql`. Only the
  CI immutability check is off: `tools/sql-migration-gate/index.ts` says
  "Suspended while open migration PRs fold into 0001_init.sql; restore
  `checkMigrationImmutability(root)` once the dev DB is wiped." The runtime
  guard stays on: `verifyChecksum` in `go/internal/store/store.go` refuses an
  edited applied migration ("applied migration v%d (%s) was edited after it
  ran").
  - While the CI check is off: edit `0001_init.sql` in place. Any database
    that already applied 0001 refuses to start until it is wiped.
  - After: add `0002_tracker_config.sql` and never edit 0001. It carries what
    0001's loops give a new table: the tenant default, ENABLE and FORCE RLS,
    the `tenant_isolation` policy, the `set_updated_at` trigger, and an
    explicit `GRANT SELECT, INSERT, UPDATE, DELETE ON tracker_config TO
    compass_app, compass_system`. 0001's grant is `ON ALL TABLES IN SCHEMA`,
    so it covers only tables that exist when 0001 runs. The precedent is
    `compass-issue-model-pr-linkage-amendment/design.md`.
- **Store errors.** Use `ErrInvalidArgument` and `ErrVersionConflict` with
  `fmt.Errorf("%w: ...")`, as `PutModelRegistry` in
  `go/internal/store/model_registry.go` does.
- **Tests.** Tenant-B cases use `seedTenant` + `WithTenant`. Do not touch the
  decisions ledger.
- **Out of scope.** The Settings UI (RIG-5008), the Linear ingest lane and
  its `issues` CHECK widen (#2078), and any outbound tracker mirror.

### T1 — Shared types and resolver (lane: compass-server)

In `go/internal/forge/linear.go`, replace the private `linearTypeCompleted`,
`linearTypeUnstarted` and `linearTypeBacklog` with exported constants, and
build `linearClosedStateTypes` from them:

```go
const (
	LinearTypeTriage    = "triage"
	LinearTypeBacklog   = "backlog"
	LinearTypeUnstarted = "unstarted"
	LinearTypeStarted   = "started"
	LinearTypeCompleted = "completed"
	LinearTypeCanceled  = "canceled"
	LinearTypeDuplicate = "duplicate"
)

// LinearStateTypes lists every Linear SDL WorkflowState.type.
var LinearStateTypes = []string{LinearTypeTriage, LinearTypeBacklog, LinearTypeUnstarted,
	LinearTypeStarted, LinearTypeCompleted, LinearTypeCanceled, LinearTypeDuplicate}
```

The `{type, want}` table in `TestMapLinearStateCoversEverySDLType` stays,
including its fallback row. Add an assertion that the table covers every
entry of `LinearStateTypes`.

New file `go/internal/store/tracker_config.go`:

```go
// TrackerStatusRule maps one Linear workflow-state name to a Compass state.
// An empty TeamKey applies to every team.
type TrackerStatusRule struct {
	TeamKey    string
	StatusName string
	State      IssueState // IssueStateBacklog..IssueStateDone
}

// TrackerConfig is a tenant's inbound status mapping.
type TrackerConfig struct {
	Rules []TrackerStatusRule
}

// DefaultTrackerConfig is what a tenant with no stored row reads: the
// any-team "In Review" -> IssueStateInReview rule.
func DefaultTrackerConfig() TrackerConfig

// ValidateTrackerConfig rejects a config the resolver cannot apply safely.
func ValidateTrackerConfig(cfg TrackerConfig) error
```

`ValidateTrackerConfig` returns `ErrInvalidArgument` for:

- more than 200 rules;
- an empty `StatusName`, or one over 256 bytes or with a control character;
- a `TeamKey` over 64 bytes or with a control character;
- a `State` outside `IssueStateBacklog`..`IssueStateDone`;
- two rules with the same (`TeamKey`, `StatusName`).

New file `go/internal/tracker/linear.go`:

```go
// LinearWorkflowState is one issue's Linear workflow state as the fetch reads it.
type LinearWorkflowState struct {
	Name string
	Type string
}

// Resolved is the Compass state plus the raw name for display.
type Resolved struct {
	State      store.IssueState
	StatusName string
}

// ResolveLinearStatus applies cfg to one issue's workflow state on teamKey:
// a team rule, then an any-team rule, then the type default. ok is false for
// an unknown type that no rule names; the caller makes no transition.
func ResolveLinearStatus(cfg store.TrackerConfig, teamKey string, ws LinearWorkflowState) (Resolved, bool)
```

Interfaces: produces the exported constants, the config types and
`ResolveLinearStatus` for #2078, T3 and T4. No dependencies. Tests:

- Every entry of `forge.LinearStateTypes` has a default, and each type
  `forge.MapLinearState` calls `closed` maps to `DONE`.
- An unknown type is `ok=false`, unless a rule names its state.
- A team rule beats an any-team rule, and an any-team rule beats the default.
  A rule for another team does not match.
- An "In Review" `started` state is `IN_REVIEW` under
  `DefaultTrackerConfig()` and `IN_PROGRESS` under an empty config.
- `StatusName` is always `ws.Name`.
- Each validation case is `ErrInvalidArgument`.

### T2 — Proto (lane: compass-server)

In `proto/compass/v1/compass.proto`, after `rpc DeleteModelRegistry`:

```proto
  // Report the tenant's tracker status mapping. A tenant that never wrote one
  // gets version 0 and the built-in default. Open to any authenticated account.
  rpc GetTrackerConfig(GetTrackerConfigRequest) returns (GetTrackerConfigResponse);

  // Replace the tenant's tracker status mapping under compare-and-set.
  // Requires a user with the admin role.
  rpc PutTrackerConfig(PutTrackerConfigRequest) returns (PutTrackerConfigResponse);
```

After `message DeleteModelRegistryResponse`:

```proto
// One inbound rule: a Linear workflow-state name maps to a Compass state.
message TrackerStatusRule {
  string team_key = 1;    // Linear team key, e.g. "RIG"; "" = every team
  string status_name = 2; // exact workflow-state name
  IssueState state = 3;   // BACKLOG..DONE; never UNSPECIFIED or ARCHIVED
}

// The tenant's inbound mapping. A state no rule names falls back to its
// Linear type.
message TrackerConfig {
  repeated TrackerStatusRule rules = 1;
}

message GetTrackerConfigRequest {}
message GetTrackerConfigResponse {
  int64 version = 1;
  TrackerConfig config = 2;
  bool can_edit = 3; // the caller may Put (a user with the admin role)
}

message PutTrackerConfigRequest {
  TrackerConfig config = 1;
  // The version the caller read. 0 seeds the first config. A stale value is
  // ABORTED: re-read and retry.
  int64 expected_version = 2;
}
message PutTrackerConfigResponse {
  int64 version = 1;
}
```

Regenerate. Classify both procedures `authenticatedOpen` in
`classifyProcedure` in `go/internal/auth/admin_gate.go`, with a comment that
the Put handler checks for a tenant admin.

Interfaces: produces the generated `compassv1` types and
`CompassService.method.getTrackerConfig` / `putTrackerConfig` for T4 and
RIG-5008.

### T3 — Schema and store (lane: compass-server)

In `0001_init.sql`, after `account_tour_state` (or in `0002`, per Global
Constraints):

```sql
-- One tracker status mapping per tenant. version is the CAS
-- substrate, as on model_registry. config is validated at the store door.
CREATE TABLE tracker_config (
    tenant_id  TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    version    BIGINT      NOT NULL,
    config     JSONB       NOT NULL,
    updated_by TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id)
);
```

Add `'tracker_config'` to the `tenant_tables` and `updated_at_tables` arrays.
Add it to the `tenantOwned` floor list in `TestRLSCatalogEnabledAndForced` in
`go/internal/store/rls_pgtest_test.go`.

New `go/internal/store/queries/tracker_config.sql`, regenerated with sqlc:

```sql
-- name: CurrentTrackerConfig :one
SELECT version, config FROM tracker_config;

-- name: InsertTrackerConfig :one
INSERT INTO tracker_config (version, config, updated_by)
VALUES (1, $1, $2)
ON CONFLICT (tenant_id) DO NOTHING
RETURNING version;

-- name: UpdateTrackerConfig :one
UPDATE tracker_config
   SET config = $1, updated_by = $2, version = version + 1
 WHERE version = $3
RETURNING version;
```

These queries have no tenant predicate. RLS narrows them to one row. Under
`WithSystemRole` (BYPASSRLS, no tenant) they would read an arbitrary tenant's
row and update every tenant at that version. `TestSessionForAccountUnderSystemRoleIsUnscoped`
in `go/internal/store/session_bindings_pgtest_test.go` pins the same hazard.
So both methods refuse that role before any query.

In `go/internal/store/tracker_config.go`:

```go
// CurrentTrackerConfig returns the caller tenant's config, or version 0 and
// DefaultTrackerConfig() when the tenant has no row. It refuses a
// system-role ctx.
func (s *Store) CurrentTrackerConfig(ctx context.Context) (version int64, cfg TrackerConfig, err error)

// PutTrackerConfig validates cfg and replaces it under CAS: 0 seeds the first
// row, N>0 lands only at version N. A lost race is ErrVersionConflict. It
// refuses a system-role ctx.
func (s *Store) PutTrackerConfig(ctx context.Context, actor AccountID, cfg TrackerConfig, expectedVersion int64) (version int64, err error)
```

Interfaces: consumes T1. Produces both methods for T4 and the consumer's
per-batch load. Pgtests:

- No row reads version 0 and the default config.
- Seed at 0, update at 1, then a stale 1 is `ErrVersionConflict`. A seed when
  a row exists is `ErrVersionConflict`.
- Tenant B reads the default after tenant A writes, and seeds its own row.
- Under `WithSystemRole`, both methods return an error and write nothing.
- An invalid config is `ErrInvalidArgument` and writes nothing.

### T4 — Service handlers (lane: compass-server)

Move the body of `secretsService.requireUser` in
`go/server/secrets_service.go` into a package function, and have
`requireUser` call it:

```go
// requireUserAccount returns the caller and role only for a user account.
// No caller is CodeUnauthenticated; an agent or a missing account is
// CodePermissionDenied with `denied` as the message.
func requireUserAccount(ctx context.Context, st *store.Store, denied string) (store.AccountID, store.UserRole, error)
```

New file `go/server/tracker_config_service.go`, on `*service`:

```go
func (s *service) GetTrackerConfig(ctx context.Context, req *connect.Request[compassv1.GetTrackerConfigRequest]) (*connect.Response[compassv1.GetTrackerConfigResponse], error)

func (s *service) PutTrackerConfig(ctx context.Context, req *connect.Request[compassv1.PutTrackerConfigRequest]) (*connect.Response[compassv1.PutTrackerConfigResponse], error)
```

- `GetTrackerConfig`: `requireCaller`, then `s.store.GetAccount` for the
  role, then `CurrentTrackerConfig`. `can_edit` is true only for a user with
  `UserRoleAdmin`. An agent or an account with no user role gets
  `can_edit=false`, never an error.
- `PutTrackerConfig`: `requireUserAccount(ctx, s.store, "tracker config writes are user-only")`; a role other than
  `UserRoleAdmin` is `CodePermissionDenied`. Then `PutTrackerConfig`. Rename
  `mapModelRegistryErr` to `mapVersionedWriteErr` and use it in both
  services.

Interfaces: consumes T2 and T3. Tests:

- A member Put and an agent Put are `PermissionDenied`.
- A stale admin Put is `Aborted`.
- An unconfigured Get is version 0 with the default rule and `can_edit`
  matching the role.
- A pgtest shows tenant B's Get does not see tenant A's rules.

### Consumer interfaces for RIG-5008 (lane: compass-ux)

This is not a task here. The Settings section consumes:

```ts
CompassService.method.getTrackerConfig  // {} -> { version: bigint, config?: TrackerConfig, canEdit: boolean }
CompassService.method.putTrackerConfig  // { config, expectedVersion } -> { version: bigint }
TrackerStatusRule                       // { teamKey: string, statusName: string, state: IssueState }
```

- **Save model:** per action, like Providers. Each add, edit or remove of a
  rule sends one Put of the whole list with the version last read.
- **Unconfigured:** version 0 shows the built-in "In Review" rule. The first
  Put sends `expectedVersion: 0`.
- **Errors:** `Aborted` means re-read and show the new rules.
- **Read-only:** with `canEdit` false, rules render as read-only rows.
- **Types:** the client `TrackerConfig.kind` and `handle` have no server
  field (OQ-2).

## Tasks

- [ ] T1: exported Linear type list in `forge`, config types and validation in `store`, `tracker.ResolveLinearStatus`, with table tests.
- [ ] T2: proto messages and `GetTrackerConfig` / `PutTrackerConfig`, regenerated and classified.
- [ ] T3: `tracker_config` table with RLS and trigger, sqlc queries, CAS store methods that refuse the system role, tenant-B pgtests.
- [ ] T4: shared `requireUserAccount`, handlers with the tenant-admin check and `can_edit`, with tests.

## Open Questions

The record is designed against each recommendation.

1. **Echo suppression without `toTracker`.** DL-129 makes an observation a
   no-op when the observed status equals `toTracker(current state)`. This
   contract has no `toTracker`. Without a replacement, every Linear edit (a
   comment, a title) re-resolves the issue. That reverts QUEUED, BLOCKED and
   IN_REVIEW, which have no Linear type, and un-archives Done issues.
   - (a) Change-only: the consumer preconditions in Approach. No `toTracker`
     is needed. This amends DL-129's mechanism. When an outbound mirror
     lands, its record adds suppression for the mirror's own echoes.
   - (b) Store a `toTracker` half now, only to compute DL-129's rule. It has
     no writer, and a non-injective map needs round-trip validation against
     the inbound rules.

   **Recommendation: (a).** Linear already reports a state change
   (`updatedFrom.stateId`), and no mirror writes board state to Linear today.
2. **Which settings the contract carries.**
   - (a) Inbound mapping only.
   - (b) Also a per-account `handle`. No server path reads it.
   - (c) Also outbound `toTracker` rules. No outbound mirror exists.

   **Recommendation: (a).** Each other field comes back with its first
   server reader. An outbound record must check its `toTracker` against
   these inbound rules.
3. **Scope.** One Linear app credential serves every tenant, but each tenant
   has its own `issues` rows.
   - (a) Per tenant. The consumer loads the config under the tenant it
     upserts the issue under. Today that is the bootstrap tenant.
   - (b) Deployment singleton with an `adminOnly` Put. It matches the
     credential's scope and needs no tenant source. A later per-tenant board
     would need a migration.

   **Recommendation: (a).** Board state is per tenant, so its mapping is
   too. The cost is RLS on one row and the system-role refusal.
4. **Unknown Linear type.**
   - (a) No transition (`ok=false`).
   - (b) `BACKLOG`, like the UI's `fromTrackerStatus` fallback.

   **Recommendation: (a).** A new Linear type should not demote live work.
   (b) turns a schema surprise into a silent state write.
5. **Rule key.**
   - (a) Workflow-state name, with an optional team.
   - (b) Workflow-state id, with the name as a label. The state list already
     exists: `(*Linear).workflowStatesFor` fetches `nodes { id name type }`
     with a TTL cache and a truncation guard. Only a thin list-states RPC and
     the UI picker are new. Ids survive renames, and Put could reject a type
     contradiction, as `resolveWorkflowState` does outbound.

   **Recommendation: (a)** for this contract. It needs no Linear call on the
   settings path, and the webhook decode needs only the name. Under OQ-1(a),
   a rename moves no card; only later moves into the renamed state get the
   type default. (b) is a later additive change.
6. **Who may Put.**
   - (a) A user with `UserRoleAdmin` in the tenant, checked in the handler.
   - (b) `adminOnly`, which admits only the bootstrap admin.

   Today both admit the same account: `BootstrapAdmin` is the only producer
   of `UserRoleAdmin`, and `CreateUser` mints members.
   **Recommendation: (a)**, matching OQ-3(a). If OQ-3 rules (b), Put becomes
   `adminOnly`.
7. **When a Put takes effect.**
   - (a) At each issue's next state change.
   - (b) Put also re-resolves every stored Linear issue.

   **Recommendation: (a).** It follows OQ-1(a). (b) writes states with no
   tracker event and needs its own echo rules.
