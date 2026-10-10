# Compass workstream repository source (RIG-4951)

## Problem / Intent

Tracker: RIG-4951. This record is A3 of the scope-enforcement rulings
amendment (RIG-4436,
[`compass-forge-scope-enforcement-rulings-amendment/design.md`](../compass-forge-scope-enforcement-rulings-amendment/design.md)),
which amends the frozen parent record
[`compass-forge-scope-enforcement/design.md`](../compass-forge-scope-enforcement/design.md).

Matt ruled the git credential covers "the workstream repository plus the
account's write grants" (OQ-7), and that "the workstream repository is a
server-side spawn-target record. No provision proto field is added" (OQ-8).
The server has no such record today. The brokered GitHub App token is built
from grants alone. `gitCredentialBroker.credential` in
`go/server/git_credential.go` reads:

```go
repos, err := b.grants.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, b.host)
```

Spawn names no target either. `runSpawn` in `go/server/spawn.go` provisions
with only an idempotency key, and `ProvisionAgentWorkspaceRequest` in
`proto/compass/v1/compass.proto` records "Repo carriage removed (RIG-1527 …)".

This record chooses where an agent's workstream repository comes from, how
the server stores it, and how it changes over the agent's life. The broker
then mints for it together with the account's grants. The record only widens
the minted set; it never narrows it below the account's grants. Agents still
self-clone after launch (DL-090). The GitHub App stays the only GitHub
credential.

## Approach

This body records Matt's rulings on RIG-4997: F1 A, F2 durable rows, and F3
no deployment seed. See **Rulings**.

### A workstream repository is a per-agent write grant

Every repository in the brokered token gets write access.
`gitCredentialPermissions` in `go/server/git_credential.go` is:

```go
"contents":      "write",
"pull_requests": "write",
```

So the record stores workstream repositories as agent-owned rows in the
existing `account_forge_scopes` table (F1). The read queries already include
an agent's own rows. `ListForgeScopeRepos` in
`go/internal/store/queries/forge_scopes.sql` reads:

```sql
LEFT JOIN agent_accounts AS agent ON agent.account_id = $1
WHERE scope.account_id IN ($1, agent.owner_user_id)
```

`HasForgeScope` uses the same predicate. The broker and the API write gate
(`requireForgeScope` in `go/server/forge.go`) both pick up the rows with no
query change, so the token and the gate cannot disagree. This is the frozen
parent's OQ-3 path: "schema remains additive for per-agent grants". Only the
foreign key blocks it today:

```sql
account_id     TEXT NOT NULL REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
```

The rows are an allowlist, never a resolver. Forge calls still name `repo`
explicitly, and nothing derives a default repo from these rows. That keeps
the ownership layer's ruling ("Ruling: it never resolves" in
`compass-server-ownership-layer/design.md`), which rejected a per-agent repo
mapping as a source for an empty `repo`.

### Producers

The rows are durable (F2). Matt rejected a deployment seed (F3): "you don't
know which agents will be spawned before you start the server. We can't land
this until we can dynamically configure it on the agents." So no boot-time
producer exists, and this record adds no spawn-time write. Whether a child
copies its parent's rows is a writer decision, deferred to RIG-5015.

The producer is a runtime per-agent write, designed in RIG-5015. It writes
through `GrantAgentForgeScope` (T2) and removes through `RevokeForgeScope`.
Spawn stays repo-free. `SpawnPeerRequest`'s comment in
`proto/compass/v1/agent_gateway.proto` says "never a repo/ref (spawn carries
no repo, Matt 2026-07-29)".

Card capture (`issues.repo` with `assignee`) has no producer yet.
`go/internal/store/issues.go` says of `assignee`: "No setter for them in
THIS slice — a setter lands with its producer". A Dispatcher writer is
deferred until its grant lifetime is decided: rows carry no producer, so a
revoke on unassignment could remove a row another writer still needs.

A `workstream_repo` column on `agent_accounts` is rejected. One column
encodes the one-repo model the ownership layer rejected.

### Tenant RLS

`account_forge_scopes` is already in the `tenant_tables` array, with the
`tenant_isolation` policy. FK checks ignore RLS, so the agent writer selects
the agent under RLS, as `GrantForgeScope` does:

```sql
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT u.account_id, sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo)
FROM user_accounts AS u
```

The Runner fetch has a tenant gap. `bearerAuth.authenticate` in
`go/internal/runnerhub/auth.go` sets no tenant. `Handler.FetchSecrets` in
`go/internal/runnerhub/handler.go` then calls
`h.resolver.ResolveFor(ctx, agent, "runner fetch")`, so the user-secret read
and the grant read both run under the bootstrap-tenant fallback.
`FetchSecrets` will scope the ctx once, after it resolves `agent`, the way
`Hub.BindLifetime` in `go/internal/runnerhub/bind_lifetime.go` does:

```go
tenant, err := binder.AccountTenant(store.WithSystemRole(ctx), account)
...
tctx := store.WithTenant(store.WithoutSystemRole(ctx), tenant)
```

### Broker: widen only

`ListForgeScopeRepos(agent)` now returns the owner's grants plus the agent's
rows. An agent row must never make the credential worse than the owner's
grants alone. The broker keeps every guard: the 500 cap, the single-owner
rule in `gitCredentialScope`, `sameRepositorySet` and singleflight. It adds
one fallback to the owner's grants, `ListForgeScopeRepos(owner)`, which
matches only the owner's rows. The fallback fires when the full set:

- fails `gitCredentialScope` (for example, a workstream repo has a different
  owner);
- exceeds 500 repositories;
- is negative-cached; or
- fails a mint deterministically.

Today `mint` negative-caches only a repository mismatch. A non-2xx mint
returns an untyped error before the cache write. `postInstallationToken` in
`go/internal/forge/githubapp.go` returns:

```go
return installationToken{}, fmt.Errorf("forge: mint installation token: status %d: %s", resp.StatusCode, ghErr.Message)
```

It will wrap a `*forge.StatusError` instead.

- **Deterministic failure.** A 404 or 422 is deterministic; a repository
  outside the installation is the expected cause [INFERENCE: GitHub's status
  for that case]. The broker negative-caches the full key for
  `gitCredentialMismatchCache`, then mints the owner key.
- **Transient failure.** A 403, 429, 5xx or transport failure keeps today's
  behaviour: `staleCredential` on the full key, with no second mint.
  Throttling therefore never doubles the mint rate.

A `*` grant dominates, as it does today.

### Lifecycle

| Event | Behaviour |
| -- | -- |
| Set | The runtime writer (RIG-5015) adds a row. Every fetch re-reads the store, and a refresh that changes the token calls `SignalSecretsVersion`, so a live session picks the repo up within one refresh cycle. A row written before spawn is seen by the first `FetchSecretsByContainer` in `Host.Start` (`go/internal/runner/host.go`). |
| Removal | `RevokeForgeScope` deletes an agent row in the same way it deletes a grant. Immediate revocation of the old token is the parent's T5 grant-removal path ("re-mints at once, delivers the new token, then revokes the old one"), and it covers agent rows unchanged. |
| Despawn | Rows survive. Despawn tears down compute, not identity, and agents are never deleted. Without a placement there is no fetch, so a surviving row mints nothing. A respawn gets the same set back. |
| Owner grant revocation | Independent: the agent's rows stay. |
| Re-mint | The cache key is the sorted full set, so agents with equal sets share one token. |

## Plan

**Rulings** (RIG-4997): F1 A, F2 durable rows, F3 no deployment seed.

**Landing gate.** T1–T4 change nothing until a writer adds agent rows, so
they land in one stack with RIG-5015's writer, never before it
(`no-inert-gating`). T5 is that writer's call into T2.

### Global Constraints

- **Public repo.** Never name private repositories, hosts, deployments or
  roadmap (`docs/concepts/self-host-and-managed.md`,
  `docs/designs/meta/oss-core-managed-boundary/design.md`). Tenant RLS is a
  core capability and may be described.
- **No proto change.** Add no field to `ProvisionAgentWorkspaceRequest`,
  `SpawnAgentRequest` or `SpawnPeerRequest` (OQ-8). Agents still self-clone
  (DL-090). The GitHub App is the only credential (OQ-6), with
  `gitCredentialPermissions` unchanged.
- **Allowlist, not resolver.** No forge call, subscription or board path
  derives a repo from these rows.
- **Widen only.** Whenever the owner's grants alone would mint, the agent
  still receives a credential that covers at least those grants.
- **Exact repos only.** An agent row is an exact GitHub repository
  (`org/name`, lowercased), never `*`. A wildcard is an owner grant decision.
- **Migrations.** `main` is in the migration-collapse window (RIG-4881 /
  RIG-4896). Only `0001_init.sql` exists, and migration immutability is off
  until the database wipe.
  - Before immutability is re-enabled: change `0001_init.sql` in place and
    add no numbered file.
  - After: add a numbered `0002_*.sql` and never edit 0001. The runner
    rejects edits with "applied migration v%d (%s) was edited after it ran",
    and `tools/sql-migration-gate` lints new files with squawk and sqruff.
  - A new table (F1-B only) lands in the same file as its RLS policy.
- **Store errors.** Use `ErrInvalidArgument` and `fmt.Errorf("%w: ...")`,
  like `ForgeScope.normalized` in `go/internal/store/forge_scopes.go`.
- **Tests.** Tenant-B cases use `seedTenant` + `WithTenant`, as
  `TestForgeScopeRejectsAgentGrantAndSeparatesTenants` does. Do not touch the
  decisions ledger.

### T1 — Let agent accounts own scope rows

**Before immutability is re-enabled:** edit the `account_forge_scopes` column
in place in `0001_init.sql`:

```sql
account_id     TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
```

**After re-enable:** add `0002_agent_forge_scopes.sql`. The constraint name
is Postgres's default for the inline reference:

```sql
ALTER TABLE account_forge_scopes DROP CONSTRAINT account_forge_scopes_account_id_fkey;
ALTER TABLE account_forge_scopes
    ADD CONSTRAINT account_forge_scopes_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE RESTRICT NOT VALID;
ALTER TABLE account_forge_scopes VALIDATE CONSTRAINT account_forge_scopes_account_id_fkey;
```

The table already has its RLS policy and grants, so neither window adds them.

Interfaces: produces the relaxed FK. `GrantForgeScope` stays user-only
because it selects `FROM user_accounts`. Test: the existing
`TestForgeScopeRejectsAgentGrantAndSeparatesTenants` still passes, which
shows an agent id is still rejected by the user writer.

### T2 — Agent writer

Add the query to `go/internal/store/queries/forge_scopes.sql` and regenerate
with sqlc:

```sql
-- name: GrantAgentForgeScope :execrows
-- Server-written workstream row; the SELECT runs under RLS.
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT a.account_id, sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo)
FROM agent_accounts AS a
WHERE a.account_id = sqlc.arg(account_id)
ON CONFLICT DO NOTHING;
```

In `go/internal/store/forge_scopes.go`:

```go
// GrantAgentForgeScope adds a server-written exact-repo row for an agent.
func (s *Store) GrantAgentForgeScope(ctx context.Context, scope ForgeScope) (added bool, err error)
```

`GrantAgentForgeScope` runs `normalized()`, rejects `*`, and requires one
`org/name` for GitHub. When it inserts zero rows, it calls the existing
`AgentAccountVisible` query to tell "already present" (`false, nil`) from
`ErrInvalidArgument` ("scope account %q is not an agent in this tenant").

Interfaces: produces the method for T5. Pgtests:

- An agent row appears in `ListForgeScopeRepos` and `HasForgeScope` for that
  agent, but not for its owner or a sibling.
- `*` and a user id are rejected.
- `RevokeForgeScope` removes an agent row.
- Tenant B gets `ErrInvalidArgument` and sees no rows.

### T3 — Broker fallback and typed mint errors

In `go/internal/forge/githubapp.go`, `postInstallationToken` returns
`fmt.Errorf("forge: mint installation token: %w", &StatusError{Status: resp.StatusCode, Message: ghErr.Message})`.
Update any test that asserts the old text.

In `go/server/git_credential.go`, the field `grants gitCredentialGrantLister`
becomes `store gitCredentialStore`, and `buildGitCredentialBroker` still
passes `st`:

```go
type gitCredentialStore interface {
	AgentOwner(ctx context.Context, agentAccountID store.AccountID) (store.AccountID, error)
	ListForgeScopeRepos(ctx context.Context, accountID store.AccountID, provider store.ForgeProvider, host string) ([]string, error)
}

// gitCredentialScopeRejected reports a mint failure that repeats for the same
// set: errGitCredentialScopeMismatch, or a *forge.StatusError with status 404 or 422.
func gitCredentialScopeRejected(err error) bool

// credentialForRepos is today's credential body after the list read.
// rejected is true for an invalid scope, the cap, a negative entry or a
// scope-rejected mint.
func (b *gitCredentialBroker) credentialForRepos(ctx context.Context, agent store.AccountID, repos []string) (tok string, ok, rejected bool)
```

`credential` passes the agent's list to `credentialForRepos`. If the result
is rejected, it calls `AgentOwner` and `ListForgeScopeRepos(owner)`, and
retries once if the owner's key differs. `mint` negative-caches when
`gitCredentialScopeRejected(err)`. `refreshCredential` drops the entry on a
scope-rejected error, the same as on a mismatch.

Interfaces: consumes `Store.AgentOwner` and `forge.StatusError`. Extend
`fakeGitCredentialGrants` with per-account lists and `AgentOwner`. Tests:

- An agent row widens the minted set.
- A cross-owner agent row mints the owner-grants token.
- A full set over 500 falls back to the owner grants.
- A 422 on the full set is negative-cached, and a second fetch makes zero
  full-set mints.
- A 429 serves the stale full-set token and makes no owner mint.
- An owner `*` dominates.
- An `AgentOwner` error on the fallback yields no credential.
- Agent rows with no owner grants still mint.

### T4 — Tenant-scoped Runner fetch

In `go/internal/runnerhub/bind_lifetime.go`:

```go
// agentTenantCtx scopes ctx to agent's tenant through the wired LifetimeBinder,
// with the system role cleared. A hub with no binder fails Unavailable.
func (h *Hub) agentTenantCtx(ctx context.Context, agent store.AccountID) (context.Context, error)
```

`Hub.BindLifetime` uses it. `Handler.FetchSecrets` calls it after it resolves
`agent` and passes the scoped ctx to `ResolveFor`. A resolve error maps to
`CodeInternal`. In production the binder is the store
(`hub.SetLifetimeBinder(st)` in `go/server/sinks.go`), so no constructor
changes.

Interfaces: consumes `LifetimeBinder.AccountTenant`. Tests:

- `FetchSecrets` resolves under the agent's tenant with the system role
  cleared (the `recordingBinder` pattern).
- A hub with no binder fails `Unavailable`.
- Existing `FetchSecrets` handler tests wire a binder.
- A pgtest shows a tenant-B agent's declared secret resolves through
  `FetchSecrets`.

### T5 — Runtime writer

RIG-5015 designs the surface, its authorization and its caller. It writes
through `GrantAgentForgeScope` and revokes through `RevokeForgeScope`. Its
pgtests show that a row written to a running agent reaches the next
`FetchSecrets`, and that a respawn keeps the rows. This record adds no seed
flag and no spawn-time write.

## Tasks

- [ ] T1: `account_forge_scopes.account_id` references `accounts`, in place in 0001 during the collapse window or in `0002_agent_forge_scopes.sql` after.
- [ ] T2: `GrantAgentForgeScope`, with tenant-B pgtests.
- [ ] T3: broker owner-grants fallback, typed mint status and negative cache, with unit tests.
- [ ] T4: `FetchSecrets` scoped to the agent's tenant.
- [ ] T5: the runtime writer from RIG-5015. T1–T4 land in its stack.

## Rulings

Matt ruled on RIG-4997:

- **F1: A.** Workstream repositories are agent rows in
  `account_forge_scopes`. The token and the API gate agree by construction,
  and agents open PRs through Compass's attributed write path (OQ-10).
  Rejected: a separate table only the broker reads, under which the token
  could push where Compass forge writes return `not_found`.
- **F2: durable rows.** The gate reads the same table, rows live under RLS,
  and a runtime writer has a store target. Rejected: config read at mint
  time with a `ParentAgentID` walk.
- **F3: no deployment seed.** The agents a server will run are not known at
  boot, so a seed is the wrong producer. Rows come only from a runtime
  per-agent write (RIG-5015).

## Open Questions

Deferred to RIG-5015. Neither changes T1–T4.

- **Inheritance.** Whether a spawned child copies its parent's agent rows.
  Copying limits reach to the subtree but makes revoke at a child lose to the
  parent's next spawn.
- **Dispatcher lifetime.** Whether an assignment-driven row ends with the
  assignment, and how its revoke spares a row another writer added.
