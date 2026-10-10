# Compass agent workstream repository writer (RIG-5015)

## Problem / Intent

Tracker: RIG-5015. The RIG-4951 record,
[`compass-workstream-repository-source/design.md`](../compass-workstream-repository-source/design.md),
stores an agent's workstream repositories as agent-owned rows in
`account_forge_scopes`. Its implementation issue is RIG-5022 (T1–T4).
RIG-5015 blocks RIG-5022: T1–T4 land in this writer's stack, never before it.
The RIG-4951 record names no producer. Its `### Producers` section says:

> The producer is a runtime per-agent write, designed in RIG-5015. It writes
> through `GrantAgentForgeScope` (T2) and removes through `RevokeForgeScope`.

Matt rejected a deployment seed: "you don't know which agents will be spawned
before you start the server. We can't land this until we can dynamically
configure it on the agents." Spawn stays repo-free (scope-enforcement OQ-8,
DL-090).

Today nothing writes a scope row while the server runs. The only writer is the
boot seed. `reconcileForgeScopeGrants` in `go/server/serve.go` reads:

```go
for _, g := range fc.ScopeGrants {
	if err := st.GrantForgeScope(ctx, g); err != nil {
```

This record designs the runtime write that adds, removes and lists one
agent's workstream repositories, and the rule that gives an agent-spawned
agent its rows with no human step. It also settles the two questions the
RIG-4951 record deferred: inheritance at spawn, and how a future Dispatcher's
rows end.

## Approach

### The authority: the owning user

An agent row adds a repository to one agent on top of its owner's grants.
`HasForgeScope` in `go/internal/store/queries/forge_scopes.sql` admits both:

```sql
WHERE scope.account_id IN ($1, agent.owner_user_id)
```

The agent's owning user writes its agents' rows (OQ-2, ruled A). Matt: "a
user granting agents repos doesn't let agents self grant." A user grant is a
human decision, not agent self-grant. The scope-enforcement record's
`## Global Constraints` says "Grants belong to an owning user and apply to
that user's agents; agent self-grant is not allowed." Agents never write rows:
an agent caller gets `PERMISSION_DENIED` for grant and revoke. The App
installation still bounds reach: a repository outside it fails the mint, and
the RIG-4951 T3 fallback mints the owner's grants.

The caller must be a user. `requireUser` in `go/server/secrets_service.go` is
the precedent:

```go
if acct.IsAgent() {
	return "", 0, connect.NewError(connect.CodePermissionDenied, errors.New("secret writes are user-only"))
}
```

The target must be an agent the caller owns. `resolveSameOwnerAgent` in
`go/internal/comms/resolve.go` is the precedent for the check and its miss:

```go
if agentOwner != owner {
	return "", notFoundHandle(store.ErrNotFound, handle)
}
```

That method belongs to `Comms`, so the new service applies the same rule in
`go/server` as `resolveOwnedAgent`. A bare handle resolves under the caller,
because `ResolveOwner` in `go/internal/store/accounts.go` says "a user owns
itself". An `owner/agent` handle resolves through `resolveQualifiedAgent` in
`go/server/service.go`, and then `acc.Agent.OwnerUserID` must equal the
caller. An unknown handle, a non-agent, another user's agent and another
tenant's agent all get one `handleNotFound(raw)`, which reads `"no account
with handle %q"`. `AgentByHandle` "never resolves or elevates a non-agent".
Tenant scope comes from the door: `BearerInterceptor` in
`go/internal/auth/interceptor.go` runs
`next(store.WithTenant(withCaller(ctx, account), tenant), req)`. A tenant-B
agent also has a tenant-B owner, so the owner check misses it.

A tenant admin has no override: it writes only its own agents' rows.

The procedures are classified `authenticatedOpen`, not `adminOnly`, because
every user must reach its own agents. The `adminOnly` gate admits only the
bootstrap admin. `adminGate.check` in `go/internal/auth/admin_gate.go` reads:

```go
if !ok || caller != g.admin {
```

**DL-442.** DL-442 OQ-2 ruled "grants come from the ForgeConfig seed plus
store operations, no admin RPC for Beta". Matt's OQ-1 ruling amends it for
agent rows: this runtime RPC writes agent rows only, and user grants still
come from the seed. This record's decision row records the amendment.

### Surface: a dedicated `AgentRepositoryService` in `go/server`

Add `AgentRepositoryService` to `proto/compass/v1/compass.proto`, beside
`SecretsService`, with `GrantAgentRepository`, `RevokeAgentRepository` and
`ListAgentRepositories`. Implement it in
`go/server/agent_repository_service.go`, next to the broker that consumes the
rows (OQ-1, ruled D).

This placement has three benefits over `CommsService`:

- **Inputs at construction.** `buildDoors` in `go/server/serve.go` already
  holds the hub and builds the broker:

  ```go
  gitCredentials, err := buildGitCredentialBroker(cfg, st, serverResolver, hub, slog.Default())
  ```

  The service takes both in its constructor. It needs no post-construction
  setter and has no unwired state. `Comms` is built before the hub, so a comms
  handler would need a setter like `SetPresenceSource`.
- **No forge coupling in comms.** The `CommsService` charter in
  `proto/compass/v1/comms.proto` is "accounts, channel groups + channels,
  messages, agent workspaces, and the event stream". A credential allowlist is
  not on that list.
- **A real precondition.** `buildGitCredentialBroker` returns nil when no
  GitHub App is configured:

  ```go
  return nil, nil //nolint:nilnil // nil broker means App not configured
  ```

  With a nil broker, the service returns `FAILED_PRECONDITION` and writes
  nothing. A row that no token can cover is never reported as added.

The host is the broker's own host (`host: rc.Host` in
`buildGitCredentialBroker`), so rows and mint lookups agree. The provider is
always GitHub, as `gitCredentialBroker.credential` reads
`store.ForgeProviderGitHub`.

An agent caller may list its own rows, as `ListSecrets` is open to agent
tokens. The `SecretsService` comment in `proto/compass/v1/compass.proto` says
"ListSecrets is callable by user AND agent tokens". A read of its own rows
widens nothing. Agents self-clone (DL-090), and nothing else tells an agent
which repositories its credential covers.

### Store path

- **Add.** `Store.GrantAgentForgeScope` (RIG-4951 T2). It selects the agent
  under RLS and returns `added=false` for an existing row.
- **Remove.** A new agent-only `Store.RevokeAgentForgeScope` that returns
  `removed`. It deletes only when the account is an agent, the same shape as
  the grant's `FROM agent_accounts`. `RevokeForgeScope` stays unchanged: its
  comment says it "removes one user grant; a missing grant is a no-op", and
  the agent surface must never delete a user grant.
- **List.** A new `Store.ListAgentForgeScopeRepos` returns only the agent's
  own rows. `ListForgeScopeRepos` also returns the owner's grants (the `IN
  ($1, agent.owner_user_id)` predicate above), which this surface cannot
  remove.

### Org check on grant

The writer cannot know the installation's repositories, but it can detect an
org mismatch. `gitCredentialScope` in `go/server/git_credential.go` rejects a
mixed-org set:

```go
} else if owner != grantOwner {
	return "", nil, false
}
```

`ScopedAppMinter.Mint` in `go/internal/forge/githubapp.go` says "scoped names
must be leaf names under the installation owner". A row in a second org makes
the full set invalid, so the RIG-4951 T3 fallback mints only the owner's
grants. The agent then loses every agent row from its token, not just the new
one. So `GrantAgentRepository` reads `ListForgeScopeRepos(agent)`. If the set
has no `*` and has an exact entry in a different org, the grant is
`FAILED_PRECONDITION` and nothing is written. Two concurrent grants in
different orgs can both pass; the result is the T3 fallback, not a wider
token, and the cost is accepted.

### Agent-spawned agents: copy the parent's rows at creation

Managers stand up managers (DL-253). `SpawnAsAccount` in
`go/server/lifecycle.go` creates a separate account with `ParentAgentID:
caller,` and then calls `provisionAndStart(ctx, created.ID, …)`. So the child
runs in its own container with its own credential. DL-134 implementers are
in-process subagents and share the manager's credential, but a spawned
manager does not.

So `Store.CreateAgent` copies the parent's own agent rows to the new agent, in
the same transaction (OQ-3, ruled C). It already runs one tenant transaction and
writes the tree edge there: `tx, err := s.beginTenantTx(ctx)`, then the
coordination hook "so the channel reconcile commits atomically with the tree
edge". The copy reads only rows whose `account_id` is the parent agent. The
owner's grants live under the owner id, so they are not copied, and the
child's set never exceeds what its owner granted the parent.

The copy runs only on a fresh creation. A spawn on a taken handle resumes
without `CreateAgent`:

```go
case errors.Is(err, store.ErrConflict):
	resp, err = l.resumeOrReject(ctx, callerOwner, req)
```

So an owner's revoke at a child is not undone by the parent's next spawn, and
no tombstone is needed. The copy is a snapshot. A row added to or removed from
the parent later does not reach existing children; the owner changes each
child. `ReparentAgent` copies nothing. The rule applies to every agent created
under a parent, including a human `CreateAgent` with `parent_handle`.

What needs a human step: a root agent (no parent) starts with only its owner's
grants until its owner runs `compass agent repo add`. Every agent spawned under
it inherits with no human step.

### Live refresh: a targeted signal after a change

Without a signal, a new row can take a long time to reach a live session.
`refreshCredential` re-mints the cached key's own repositories and does not
read the store:

```go
for qualified := range strings.SplitSeq(key, ",") {
```

`mint` sets `refreshAt: expiresAt.Add(-gitCredentialRefreshLead)` with
`gitCredentialRefreshLead = 15 * time.Minute`, and `refreshSnapshot` signals
only `if changed && b.signal != nil`. So a session with a cached key sees the
new row only after that key's refresh changes the token. A session with no
cached key (an agent whose owner has no grants) has nothing to refresh, so it
never sees the row without a signal or a reconnect.

So after a change (`added` or `removed`), the handler signals that agent's
session only. `SignalSecretsVersion` in
`go/internal/runnerhub/secrets_signal.go` pushes to every live session in
every tenant:

```go
for sessionID := range h.sessionAccounts {
```

Each one then re-fetches and re-materializes. An agent row changes one
agent's set, so a new `Hub.SignalSecretsVersionFor` uses
`SessionForAccount(ctx, account)` and pushes one `SecretsVersion`. The Runner's
`SecretsVersion` arm in `go/internal/runner/dispatch.go` calls
`d.host.RefreshSecrets(ctx, sessionID)`, and `credential` re-reads the set. The
signal is best-effort: a failure is logged and never returned, as in
`bumpSecretsVersion`. No signal is sent for a no-op write or a list.

### Transient mint failure keeps the previous token

After a grant, the agent's set is a new cache key with no entry. If that mint
fails with 403, 429, 5xx or a transport error, `credential` falls back to
`tok := b.staleCredential(key)`, and `staleCredential` returns `""` for a key
with no entry. The materializer then removes the agent's working token.
`SecretMaterializer.Install` in `go/internal/runtime/secrets_materialize.go`
runs `removeGHHostsScript(homeDir)` when no GH credential resolves.

So the broker records each agent's last delivered key. On a transient failure
of a key with no usable stale token, it serves `staleCredential` of the
agent's last key. It makes no extra mint, so the RIG-4951 rule "Throttling
therefore never doubles the mint rate" holds.

### Removal

After a revoke and a signal, the next fetch mints the narrower key. If the set
becomes empty, `Install` removes `hosts.yml`.

The old token is not revoked. The parent record's removal path ("revokes the
old one with `DELETE /installation/token`") is not built:
`forge.ScopedAppMinter` has only `Mint`. A removed repository stays reachable
through the old token until it expires, at most one hour (`mint` falls back
to `expiresAt = now.Add(time.Hour)` for an unset expiry). This record accepts
that window (OQ-4, ruled A).

### Dispatcher

This record adds no Dispatcher writer and no producer column (OQ-5, ruled A). Rows
carry no producer today. A producer column with no second writer would be
inert (`no-inert-gating`).

## Plan

### Global Constraints

- **Public repo.** Never name private repositories, hosts, deployments or
  roadmap (`skill://compass-managed-boundary`). Tenant RLS is a core
  capability and may be described.
- **Stack.** One linear stack. RIG-5022 (RIG-4951 T1–T4) is at the bottom,
  then T1–T6 of this record. RIG-4951 T5 is this record. No PR in the stack
  merges alone.
- **No spawn proto change.** Add no field to `ProvisionAgentWorkspaceRequest`,
  `SpawnAgentRequest` or `SpawnPeerRequest` (scope-enforcement OQ-8). Agents
  self-clone (DL-090). The GitHub App is the only credential (DL-441), and
  `gitCredentialPermissions` is unchanged.
- **Allowlist, not resolver.** No forge call, subscription or board path
  derives a repository from these rows.
- **Exact repositories only.** An agent row is one GitHub `org/name`,
  lowercased, never `*`. A wildcard is an owner grant decision.
- **Owner authority.** Only the agent's owning user writes its rows. A tenant
  admin has no override. An agent caller is `PERMISSION_DENIED` for grant and
  revoke before any target lookup. The copy at creation never writes a row its
  parent does not hold.
- **Oracle-safe misses.** An unknown, non-agent, other-owner or other-tenant
  target is one `NOT_FOUND` that names the submitted handle (`handleNotFound`
  in `go/server/service.go`).
- **Exhaustive classification.** Every new procedure is classified in
  `classifyProcedure`, or `classify_exhaustive_test.go` in `go/internal/auth`
  fails (it ranges `File_compass_v1_compass_proto`).
- **Migrations.** This record adds no migration. RIG-4951 T1 owns the FK
  change that lets agent rows exist.
- **Store errors.** Use `ErrInvalidArgument` with `fmt.Errorf("%w: ...")`, like
  `ForgeScope.normalized` in `go/internal/store/forge_scopes.go`.
- **Tests.** Tenant-B cases use `seedTenant` and `WithTenant`, as
  `TestForgeScopeRejectsAgentGrantAndSeparatesTenants` does. Generated code
  comes from the `buf.gen*.yaml` configs; never hand-edit `go/gen` or the TS
  `gen` trees. Do not touch the decisions ledger.

### T1 — Store: agent revoke, agent list, copy at creation

In `go/internal/store/queries/forge_scopes.sql`, add:

```sql
-- name: RevokeAgentForgeScope :execrows
-- Agent rows only: a user id deletes nothing, so a user grant is never removed here.
DELETE FROM account_forge_scopes
WHERE account_id IN (SELECT a.account_id FROM agent_accounts AS a WHERE a.account_id = $1)
  AND forge_provider = $2 AND forge_host = $3 AND repo = $4;

-- name: ListAgentForgeScopeRepos :many
-- An account's own rows only; the owner's grants are not included.
SELECT scope.repo
FROM account_forge_scopes AS scope
WHERE scope.account_id = $1
  AND scope.forge_provider = $2
  AND scope.forge_host = $3
ORDER BY scope.repo;

-- name: CopyAgentForgeScopes :exec
-- A new child starts with its parent agent's own rows; runs under RLS.
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT sqlc.arg(child_id), scope.forge_provider, scope.forge_host, scope.repo
FROM account_forge_scopes AS scope
WHERE scope.account_id = sqlc.arg(parent_id)
ON CONFLICT DO NOTHING;
```

Regenerate with sqlc. In `go/internal/store/forge_scopes.go`:

```go
// RevokeAgentForgeScope removes one agent row; removed is false when none existed.
func (s *Store) RevokeAgentForgeScope(ctx context.Context, scope ForgeScope) (removed bool, err error)

// ListAgentForgeScopeRepos returns an account's own rows, not its owner's grants.
func (s *Store) ListAgentForgeScopeRepos(ctx context.Context, accountID AccountID, provider ForgeProvider, host string) ([]string, error)
```

`RevokeAgentForgeScope` runs `normalized()`. `ListAgentForgeScopeRepos`
validates its arguments as `ListForgeScopeRepos` does. In `Store.CreateAgent`
(`go/internal/store/accounts.go`), when `a.ParentAgentID != ""`, run
`qtx.CopyAgentForgeScopes` after `InsertAgentAccount` and before commit.

Interfaces: consumes RIG-4951 T1 (FK widening) and T2. Produces the three
methods for T4 and the copy for T6. Pgtests:

- `RevokeAgentForgeScope` returns `true` once, then `false`.
- `RevokeAgentForgeScope` with a user id and that user's grant returns
  `false`, and the grant stays.
- `ListAgentForgeScopeRepos(agent)` returns the agent's rows and not the
  owner's grants. `ListForgeScopeRepos(agent)` still returns both.
- `CreateAgent` with a parent copies the parent's agent rows and not the
  owner's grants. A root agent gets no rows.
- Under tenant B, the revoke deletes nothing and the list is empty.

### T2 — Hub: per-account secrets signal

In `go/internal/runnerhub/secrets_signal.go`:

```go
// SignalSecretsVersionFor pushes one SecretsVersion to account's live session.
// No live session, or no Runner, is a nil no-op.
func (h *Hub) SignalSecretsVersionFor(ctx context.Context, account store.AccountID) error
```

It resolves the session with `SessionForAccount`, mints one version with
`mintSecretsVersion`, and pushes through the Runner router, the same command
shape as `SignalSecretsVersion`.

Interfaces: produces the method for T4. Tests in
`go/internal/runnerhub/secrets_test.go`:

- With sessions for A and B, only A's session gets a `SecretsVersion`.
- With no session for A, or no Runner, the call returns nil and pushes
  nothing.
- The version is greater than a prior `SignalSecretsVersion` version.

### T3 — Broker: serve the agent's last key on a transient failure

In `go/server/git_credential.go`, add `last map[store.AccountID]string` to
`gitCredentialBroker`, guarded by `mu`. Record the key each time `credential`
returns a token for an agent. On a mint error that is not scope-rejected
(RIG-4951 T3's `gitCredentialScopeRejected`), when `staleCredential(key)` is
empty, return `staleCredential(b.last[agent])`. `refreshDue` deletes `last`
entries whose key it evicts.

Interfaces: consumes RIG-4951 T3. Tests in `go/server/git_credential_test.go`
with `fakeGitCredentialMinter`:

- The owner set mints. An agent row is added, the minter returns 429 for the
  widened set, and the next `credential` returns the owner-set token. The
  minter's `calls` grows by exactly one.
- A 422 on the widened set still takes the RIG-4951 T3 owner fallback.
- After the last key is evicted, a transient failure yields no credential.

### T4 — `AgentRepositoryService`

In `proto/compass/v1/compass.proto`, after `SecretsService`:

```proto
// Workstream repositories of one agent: exact GitHub repositories added to
// the agent's credential and forge write grants. Writes require the agent's
// owning user; an agent may list its own rows.
service AgentRepositoryService {
  // Add one `org/name`. FAILED_PRECONDITION when no GitHub App is configured
  // or the org differs from the agent's credential org.
  rpc GrantAgentRepository(GrantAgentRepositoryRequest) returns (GrantAgentRepositoryResponse);
  // Remove one `org/name`. removed reports whether the row existed.
  rpc RevokeAgentRepository(RevokeAgentRepositoryRequest) returns (RevokeAgentRepositoryResponse);
  // List the agent's own rows, not its owner's grants. An agent caller sends
  // an empty agent_handle and gets its own rows.
  rpc ListAgentRepositories(ListAgentRepositoriesRequest) returns (ListAgentRepositoriesResponse);
}

message GrantAgentRepositoryRequest {
  string agent_handle = 1; // bare (the caller's agent) or `owner/agent`
  string repository = 2;   // `org/name`; never `*`
}
message GrantAgentRepositoryResponse { bool added = 1; }
message RevokeAgentRepositoryRequest {
  string agent_handle = 1;
  string repository = 2;
}
message RevokeAgentRepositoryResponse { bool removed = 1; }
message ListAgentRepositoriesRequest { string agent_handle = 1; }
message ListAgentRepositoriesResponse { repeated string repositories = 1; }
```

Regenerate. Classify the three procedures `authenticatedOpen` in
`classifyProcedure`, with a comment that the handler checks the caller kind
and the owner.

In `go/server/service.go`, turn `(*service).resolveQualifiedAgent` into a
package function and update its three callers (`ProvisionAgentWorkspace`,
`IssueToken`, `SpawnAgent`):

```go
func resolveQualifiedAgent(ctx context.Context, st *store.Store, raw string) (store.Account, error)
```

New file `go/server/agent_repository_service.go`:

```go
// agentSecretsSignaler is the narrow hub surface: one agent's re-fetch prod.
type agentSecretsSignaler interface {
	SignalSecretsVersionFor(ctx context.Context, account store.AccountID) error
}

type agentRepositoryService struct {
	store  *store.Store
	host   string // empty when no GitHub App is configured
	signal agentSecretsSignaler
}

func newAgentRepositoryService(st *store.Store, broker *gitCredentialBroker, signal agentSecretsSignaler) *agentRepositoryService

// agentRepository lowercases one GitHub `org/name` and rejects anything else,
// including `*`, with store.ErrInvalidArgument.
func agentRepository(raw string) (string, error)

// resolveOwnedAgent resolves a bare handle under caller, or an `owner/agent`
// handle, to an agent caller owns. Every other outcome is handleNotFound(raw).
func resolveOwnedAgent(ctx context.Context, st *store.Store, caller store.AccountID, raw string) (store.Account, error)
```

The constructor sets `host` from `broker.host` when the broker is non-nil. A
nil hub must land as a nil interface, as `newService` guards. Grant and revoke
run in order:

1. No caller → `UNAUTHENTICATED`. A caller that is not a user (an agent, or
   the system account) → `PERMISSION_DENIED` ("agent repository changes are
   user-only"), read with `GetAccount` as `requireUser` does.
2. `host == ""` → `FAILED_PRECONDITION` ("GitHub App not configured").
3. An empty handle, or an `agentRepository` error → `INVALID_ARGUMENT`.
4. `resolveOwnedAgent` → `NOT_FOUND` naming the submitted handle.
5. Grant only: the org check against `ListForgeScopeRepos(agent)` →
   `FAILED_PRECONDITION`.
6. `GrantAgentForgeScope` or `RevokeAgentForgeScope` with
   `Provider: store.ForgeProviderGitHub, Host: s.host`. On `added` or
   `removed`, call `SignalSecretsVersionFor(ctx, agent)` and log a failure with
   `slog.WarnContext`.

`ListAgentRepositories` runs step 2 first. An agent caller with an empty
handle lists itself, and an agent caller with any handle is
`PERMISSION_DENIED`. A user runs steps 3 and 4 for the handle, then
`ListAgentForgeScopeRepos`. A caller that is neither is `PERMISSION_DENIED`.

In `buildDoors` (`go/server/serve.go`), move the `buildGitCredentialBroker`
call above the socket door. Build `newAgentRepositoryService(st,
gitCredentials, hub)` and mount it on the socket door (ambient identity, as
`SecretsService`), the dev door (admin gate plus ambient identity) and the
network door. `buildNetworkServer` in `go/server/network_door.go` takes the
handler as a new parameter; update its callers.

Interfaces: consumes T1, T2, `GrantAgentForgeScope` and `ListForgeScopeRepos`.
Produces the RPCs for T5. Pgtests in
`go/server/agent_repository_service_pgtest_test.go`, with a recording
signaler:

- The owner adds a repository to its agent: `added=true`, one signal for that
  agent, and the row is listed. A repeat add is `added=false` with no signal.
- The owner names its agent bare and as `owner/agent`; both resolve.
- A mixed-case repository is stored lowercased. `*`, `org`, `org/` and
  `a/b/c` are `INVALID_ARGUMENT`.
- A user naming another user's agent gets `NOT_FOUND` naming the handle for
  grant, revoke and list, and no row is written. A `UserRoleAdmin` user
  naming a member's agent gets the same `NOT_FOUND`.
- An agent caller is `PERMISSION_DENIED` for grant and revoke, including on
  itself. It lists its own rows with an empty handle, and gets
  `PERMISSION_DENIED` for any handle.
- An unknown handle and a user's handle return `NOT_FOUND` naming the handle.
- A tenant-B user naming a tenant-A agent gets `NOT_FOUND`.
- A repository in a second org is `FAILED_PRECONDITION` and writes nothing.
  With an owner `*` grant, it is accepted.
- A nil broker gives `FAILED_PRECONDITION` and writes nothing.
- Revoke returns `removed=true` with one signal, then `removed=false` with
  none.
- `classify_exhaustive_test.go` passes.

### T5 — CLI `compass agent repo`

New file `go/cmd/compass/agent_repo.go`. Add `newAgentRepoCmd()` to
`newAgentCmd` in `go/cmd/compass/agent.go`, and name the verb in its `Short`.
Add `dialAgentRepositoryClient` to `go/cmd/compass/client.go`, modeled on
`dialSecretsClient`:

```go
func newAgentRepoCmd() *cobra.Command // "repo": add <agent> <org/name>, remove <agent> <org/name>, list <agent>; <agent> is bare or owner/agent

func runAgentRepoAdd(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent, repo string, out io.Writer) error
func runAgentRepoRemove(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent, repo string, out io.Writer) error
func runAgentRepoList(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent string, out io.Writer) error
```

Output, one line each: `added <org/name>` or `already present <org/name>`;
`removed <org/name>` or `not present <org/name>`; for list, one repository per
line.

Interfaces: consumes T4. Tests use a fake server, as `secret_test.go` does
with `startFakeSecretsServer`. They assert the request fields, the exact
output of each branch, and that a server error is returned, not printed as
success. On the socket door the caller is the bootstrap admin, so the verb
changes that user's agents. Another user runs it on the network door with its
own token through `--token-file`; `IssueToken` mints one for a bare user
handle.

### T6 — End-to-end proof and operator docs

New pgtest `go/server/agent_repository_pgtest_test.go`. Build
`brokeredSecretResolver` over the real store with `fakeGitCredentialMinter`,
and grant the owner one repository. Then:

- `ResolveFor(agent, gitCredentialReason)` mints the owner set.
- After `GrantAgentRepository`, the next `ResolveFor` mints the owner set plus
  the agent's repository (the minter's `repos` records it).
- After `RevokeAgentRepository`, the next `ResolveFor` mints the owner set.
- `SpawnAsAccount` from that agent creates a child whose first `ResolveFor`
  includes the parent's repository.
- After a revoke at the child, a second `SpawnAsAccount` with the same handle
  resumes, and the child's row stays removed.
- A despawn (`DeleteAgentPlacement`) leaves the rows, so a respawn gets the
  same set.

In `docs/self-host.md`, after "Grants name a user account. That user's agents
inherit them.", add a short section: the CLI verbs, owner-only writes with the
owning user's own token, exact `org/name` in the agent's org, inheritance by
children created later, the live signal, and the up-to-one-hour window for the
old token after a remove.

Interfaces: consumes T1–T5. Produces no code interface.

## Tasks

- [ ] T1: `RevokeAgentForgeScope`, `ListAgentForgeScopeRepos`, and the parent-row copy in `CreateAgent`, with tenant-B pgtests.
- [ ] T2: `Hub.SignalSecretsVersionFor`, with unit tests.
- [ ] T3: broker serves the agent's last key on a transient mint failure.
- [ ] T4: `AgentRepositoryService` (owner-only writes, agent self-list, org check, App precondition, targeted signal), classified and mounted on all three doors.
- [ ] T5: `compass agent repo add|remove|list`, with fake-server tests.
- [ ] T6: end-to-end pgtest (grant, revoke, spawn inheritance, despawn) and the `docs/self-host.md` section.

## Alternatives considered

The Rulings cover the surface, the authority and inheritance. The smaller
choices:

- **Transient mint failure.** Accepting the outage and pinning it with a test
  was rejected: the owner adding a repository during GitHub throttling would
  strip the agent's git access.
- **Fleet-wide signal.** Reusing `SignalSecretsVersion` was rejected: each
  agent-row change would make every session in every tenant re-fetch.
- **Org mismatch.** Returning `added` with a warning flag was rejected: the
  mixed set drops every agent row from the token, not only the new one.
- **Revoke reuse.** Changing `RevokeForgeScope` to return `removed` was
  rejected: the agent surface could then delete a user grant.

## Rulings

Matt ruled on RIG-5064 (2026-10-10): D for OQ-1, A for OQ-2, C for OQ-3, and
"lgtm" for OQ-4 and OQ-5. No open questions remain.

- **OQ-1 — Writer surface: D.** A dedicated `AgentRepositoryService` in
  `go/server`, beside the broker. Rejected: `CommsService` RPCs (a
  post-construction setter and forge config in comms), an `adminOnly`
  `CompassService` RPC (the gate admits only the bootstrap admin), and an
  agent-gateway tool (agents would change credentials). This ruling amends
  DL-442 OQ-2 ("no admin RPC for Beta") for agent rows; this record's
  decision row records it.
- **OQ-2 — Who writes agent rows: A, the owning user.** Matt: "A, that
  doesn't widen, a user granting agents repos doesn't let agents self grant."
  Agents never write rows, and a tenant admin has no override on other users'
  agents. Rejected: a tenant admin only, and the owner within an
  operator-managed allowlist.
- **OQ-3 — Agent-spawned agents: C, copy at creation.** `CreateAgent` copies
  the parent agent's own rows on a fresh creation only; a resume does not
  copy, so a revoke at a child sticks. Rejected: no inheritance (an owner step
  for every agent), and an attenuated delegation tool (a new agent-facing
  surface).
- **OQ-4 — Old token after a remove: A.** Accept the up-to-one-hour window and
  document it. Rejected: building `DELETE /installation/token` now. The
  RIG-4951 `### Lifecycle` table says "agents with equal sets share one
  token", so a revoke would cut off every agent with that set.
- **OQ-5 — Dispatcher lifetime: A, defer.** When a Dispatcher writer lands, it
  adds a producer column to the primary key, so each writer removes only its
  own row. Rejected: never revoking (reach grows with each assignment) and a
  reference count (it cannot name the writer that holds a row).
