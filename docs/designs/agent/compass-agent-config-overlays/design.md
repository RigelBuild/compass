# Compass agent config overlays — tenant and user layers

Refs: RIG-3112

> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../../agent/compass-agent-config-delivery/design.md` header).

**Named decisions this record introduces:** D1–D12 in
[Decisions](#decisions). The driver claims ledger ids with `bun tools/dl-claim`
and lands the rows under `docs/designs/decisions/agent/` in this same PR.

## Problem / Intent

Agent config is one fleet-wide bundle. `agent_config_bundle` is a singleton
row, and the fetch path reads it with no agent key. Every agent in every
tenant gets the same skills, MCP configs, rules, and settings. Tenants and
users cannot add their own config. This record keeps the operator-shipped
bundle as the global **default** layer and adds two scoped layers on top:
a **tenant** overlay and a **user** overlay. The Server resolves one
effective bundle per agent and delivers it over the existing fetch and
mount spine.

## Approach

### Ground truth

- **Default layer.** `agent_config_bundle`
  (`go/internal/store/migrations/0001_init.sql`) is a credential-free
  singleton with no `tenant_id`. It is in the RLS-exempt `bucketA` allow-list
  in `go/internal/store/rls_pgtest_test.go` (`"agent_config_bundle": true`),
  so `compass_app` reads it under any tenant.
- **Writers and roles.** `PutAgentConfig`/`DeleteAgentConfig` are `adminOnly`
  in `classifyProcedure` (`go/internal/auth/admin_gate.go`); `AdminGate.check`
  admits one account (`if !ok || caller != g.admin`), the bootstrap admin.
  `CreateUser` writes `Role: int16(UserRoleMember)`; only `BootstrapAdmin`
  writes `Role: int16(UserRoleAdmin)` (`go/internal/store/accounts.go`).
- **User coordinate.** `agent_accounts.owner_user_id TEXT NOT NULL REFERENCES
  user_accounts (account_id)` is set at insert and never updated.
  `SecretRecordsForAgent` (`go/internal/store/queries/secrets.sql`) already
  layers on it: `OR (s.scope_kind = 1 AND s.scope_id = a.owner_user_id)`.
  There is no other user model; the user layer is the agent's owning user.

### Design

**Layers and order (D1, assumes OQ-4 A).** Effective bundle =
default → tenant → user. The tenant is the agent's tenant; the user is
`agent_accounts.owner_user_id`. A missing layer is skipped. The Server
resolves the layers into one `current/` tree. The delivery record's post-MVP
seam ("config keys on a persona / agent-level role, never an individual agent
id", CD-1) stays open: a user is not an agent id. A role-keyed layer would
later add a key column to the overlay table and a slot to the version tuple
(an epoch bump, D9). A profile is not a layer (D6).

**Storage (D2).** One tenant-owned table holds whole-bundle overlays, shaped
like `account_handles` (nullable owner FK plus two partial unique indexes):

```sql
CREATE TABLE agent_config_overlays (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
                  REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_user_id TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT, -- NULL = tenant layer
    version       TEXT NOT NULL,
    bundle        BYTEA NOT NULL,
    declared_by   TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX agent_config_overlays_tenant_key ON agent_config_overlays (tenant_id) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX agent_config_overlays_user_key ON agent_config_overlays (tenant_id, owner_user_id) WHERE owner_user_id IS NOT NULL;
```

ENABLE + FORCE RLS with the standard `tenant_isolation` policy; the table
joins `tenant_tables` and `updated_at_tables`. A user write always sets
`owner_user_id = caller`, so the owner is in the writer's tenant.

**Merge rules (D3, assumes OQ-1 B).**

| Member | Rule |
|---|---|
| `settings/config.yml` | Not merged on the Server. Each layer's file is emitted as `settings/config.yml`, `settings/tenant.yml`, `settings/user.yml` (D5). |
| `models.yml` | YAML deep-merge on the Server: objects recurse, arrays and scalars replace. The SDK reads one `models.yml` path, so it cannot layer it. No key normalization. |
| `mcp/*.json` | Kept per layer as `mcp/<layer>-<name>.json` (`default-` < `tenant-` < `user-` in sort order), so `readMountedMcpConfigs` merges by server name, later wins, and keeps per-file provenance. Drops follow D4. |
| `AGENTS.md` | Concatenate default, tenant, user with a blank line between. |
| `skills/<name>/`, `extensions/<name>/` | Whole directory from the highest layer that has it. |
| `rules/*`, `agents/*`, `prompts/*` | Whole file by path from the highest layer that has it. |
| `profiles/` | Default layer only (D6). |

With no overlay row for the agent, the effective bundle is the default bundle
bytes, unchanged.

**Removal (D4, assumes OQ-2 B).** Overlays add or replace members. Removal:

- **MCP.** When an overlay applies, the resolver removes every server whose
  winning merged entry has `"enabled": false` from every layer file. It must
  run here: the agent connects through `MCPManager.connectServers`, which
  ignores `enabled`. With no overlay the default bytes pass through and a
  default `enabled: false` stays inert, as today (byte-identical default).
- **Rules.** `ttsr.disabledRules` in any settings layer (the SDK applies it).
- **Skills and extensions.** `cli.ts` passes `skills: mounted.skills` and
  `disableExtensionDiscovery: true`, so `createAgentSession` (`sdk.ts`) skips
  `skills.ignoredSkills` (`options.skills !== undefined`) and
  `disabledExtensions`. compass-agent therefore filters `mounted.skills` and
  the extension entry paths by the merged `Settings` before
  `createAgentSession`, using the SDK's extension id rule. The knobs then
  work at every layer, the default included.

**Settings layered by the SDK (D5).** `#loadOverlayYaml` (`settings.ts`) runs
`#migrateRawSettings` per file before `#deepMerge`, so a Server-side raw-YAML
merge could invert layer order when two layers spell one setting with legacy
and current keys. The effective tree therefore carries one settings file per
layer, and `buildFleetSettings` (`packages/compass-agent/src/cli.ts`) passes
those present as `configFiles`, in layer order. The put door still accepts
only `settings/config.yml`; only the effective validator accepts
`settings/tenant.yml` and `settings/user.yml`.

**One door, bounded sizes (D6).** An overlay write runs the
`validateAndHashConfigBundle` door: archive shape, member allow-list,
`rejectCredentialSettings`, `rejectCredentialModels`, MCP JSON validity. CD-3
holds per layer, and a merge of clean layers stays clean because each value
comes from one layer. Overlays may not carry `profiles/`, so the registry's
reverse orphan guard in `PutAgentConfig` stays default-only. A profile pins
`agents/` keys; an overlay that replaces one wins by design.
`COMPASS_PROFILE` selects from the effective bundle after layering.

Caps: the default door keeps 64 MiB / 4096 files; each overlay caps at 8 MiB
/ 512 files; the Runner `maxConfigBytes` (`go/internal/runner/config_materialize.go`)
rises to 96 MiB, above the 80 MiB sum (`maxConfigFiles` 10 000 covers 5120).
A TENANT write also validates (default, tenant), and a USER write validates
(default, tenant, user), as an effective bundle (`InvalidArgument` on
failure). The fetch path re-validates and fails closed with `CodeInternal`.

**Keyed fetch, one snapshot (D7).** `FetchAgentConfigRequest`
(`proto/compass/v1/runner.proto`) gains a required `container_name = 2`; the
unkeyed fetch is deleted. `Handler.FetchAgentConfig` mirrors `FetchSecrets`:
`AccountForContainer` → `accountTenantContext`
(`go/internal/runnerhub/bind_lifetime.go`) →
`EffectiveAgentConfig(ctx, agent, ifVersion)`. `beginTenantTx` is READ
COMMITTED (a new snapshot per statement), so one statement returns all layer
rows:

```sql
SELECT 1 AS layer, version, bundle FROM agent_config_bundle WHERE singleton
UNION ALL
SELECT CASE WHEN o.owner_user_id IS NULL THEN 2 ELSE 3 END, o.version, o.bundle
  FROM agent_config_overlays o JOIN agent_accounts a ON a.account_id = $1
 WHERE o.owner_user_id IS NULL OR o.owner_user_id = a.owner_user_id;
```

A version-only variant (no `bundle` column) answers `if_version`. On a miss
the full statement runs and the version is recomputed from its rows, so
version and bytes share one snapshot. A match costs the existing system-role
`AccountTenant` lookup plus one tenant statement.

**Materialize at Start (D8).** `Host.Provision` (`go/internal/runner/host.go`)
materializes before `h.runtime.Launch`, but `Hub.Provision` binds the
container only after the relay returns, so a keyed fetch there has no
binding. Provision now only runs `ensureRoot`. Start fetches and materializes
before exec, next to `FetchSecretsByContainer`; `promoteSession` keeps the
binding for resume Starts. Podman uses the live `MountLabel` (the create-time
`:Z` relabel misses later version dirs); the host tier is unchanged; vsock is
skipped, as `refreshOneContainer` skips it. The `CodeFailedPrecondition` "no
config surface" degrade stays; any other fetch error fails the Start.

**Effective version (D9).** With no overlay row it is the default version.
Otherwise it is lowercase hex sha256 over the length-framed tuple
(`configMergeEpoch`, default, tenant, user versions; `""` when absent).
Bumping the `configMergeEpoch` constant on any merge-rule change makes
Runners re-fetch. Hex is required: `ConfigMaterializer` uses the version as a
path segment. If `if_version` matches but `root/<version>` is missing,
`Materialize` re-fetches with an empty `if_version`.

**Refresh signal (D10).** `SignalConfigVersion(version)` becomes
`SignalConfigChanged()`, a fleet-wide nudge with no payload and no tenant
data. Every default or overlay put and delete calls it. The Runner config
worker refreshes each container with its tracked version as `if_version`.
`refreshOneContainer` fetches before `MountLabel`; on a version-only response
it skips the podman inspect, relabel, and Reload.

**Write surface (D11, assumes OQ-3 A).** The default RPCs stay unchanged and
`adminOnly`. New `authenticatedOpen` RPCs carry the overlays, each request
with a required `AgentConfigLayer layer`:

```proto
enum AgentConfigLayer {
  AGENT_CONFIG_LAYER_UNSPECIFIED = 0;
  AGENT_CONFIG_LAYER_DEFAULT = 1;  // reported by the effective read only
  AGENT_CONFIG_LAYER_TENANT = 2;
  AGENT_CONFIG_LAYER_USER = 3;
}
rpc PutAgentConfigOverlay(PutAgentConfigOverlayRequest) returns (PutAgentConfigOverlayResponse);
rpc DeleteAgentConfigOverlay(DeleteAgentConfigOverlayRequest) returns (DeleteAgentConfigOverlayResponse);
rpc GetAgentConfigOverlayInfo(GetAgentConfigOverlayInfoRequest) returns (GetAgentConfigInfoResponse);
```

Only TENANT and USER are accepted; anything else is `InvalidArgument`. USER
writes the caller's own layer. TENANT requires `UserRoleAdmin`, checked after
`requireUser` as the user-secret store does for `SecretScope`. Today only the
bootstrap tenant has an admin, so the tenant layer is writable only there
until a path that grants `UserRoleAdmin` in other tenants exists; that path is
out of scope. Agents have no write door.

**Effective read (D12).** `GetEffectiveAgentConfigInfo(agent_handle)`
(`authenticatedOpen`) resolves the caller's agent with
`AgentByHandle(ctx, caller, handle)`, runs the one-snapshot resolve, and
returns the effective version plus each member path with its winning layer.

**Trust boundary.** A user layer reaches only that user's agents; a tenant
layer reaches only its tenant and needs a tenant admin. Code-bearing members
(extensions, MCP `!command` values) run only for agents the writer controls.

## Alternatives considered

- **Member-granular overlay rows.** Rejected: a new per-member API and
  validator, and no atomic whole-layer push.
- **Layered mount, merge in the agent.** Workable (compass-agent owns the MCP
  connect; the SDK layers `configFiles`). Rejected: Server-side resolution
  keeps one `current/` tree, leaves the materializer unchanged, and yields
  one validated bundle to check and report (D12). D5 keeps this hybrid for
  settings alone, where agent-side layering is cheaper.
- **Pending binding at Provision.** Rejected: a second binding lifecycle;
  Start already holds one.
- **Merged-content hash as version.** Rejected: every `if_version` check would
  merge three tarballs.

## Plan

### Global Constraints

- Public repo: no private-product detail. The capability is "multi-tenant in
  one deployment" (`docs/designs/meta/oss-core-managed-boundary/design.md`).
- Schema: edit `go/internal/store/migrations/0001_init.sql` in place; the
  table joins `tenant_tables`, `updated_at_tables`, and `tenantOwned` in
  `go/internal/store/rls_pgtest_test.go`. Never set `updated_at` by hand.
- RLS: overlay, owner, and default reads run as `compass_app` under the tenant
  GUC. The only system-role step on the fetch path is the existing
  `accountTenantContext` `AccountTenant` lookup `FetchSecrets` uses.
- No overlay passes a weaker door than `PutAgentConfig`.
- Clean cutover: no unkeyed `FetchAgentConfig`, `SignalConfigVersion`, or
  Provision-time fetch remain; migrate the fakes and test clients
  (`go/internal/runnerhub/config_fetch_test.go`,
  `go/server/runner_config_compose_pgtest_test.go`).
- Red → green: each task lands its failing tests first.

### T1 — Store: table, merge, resolve

```go
// go/internal/store/agent_config_overlay.go — tenant from ctx; owner "" = tenant layer.
func (s *Store) PutAgentConfigOverlay(ctx context.Context, actor, owner AccountID, bundle []byte) (version string, err error)
func (s *Store) DeleteAgentConfigOverlay(ctx context.Context, owner AccountID) error
func (s *Store) AgentConfigOverlayInfo(ctx context.Context, owner AccountID) (AgentConfigInfoResult, error)
// bundle nil on an ifVersion match; else the one-statement full read.
func (s *Store) EffectiveAgentConfig(ctx context.Context, agent AccountID, ifVersion string) (version string, bundle []byte, err error)
func (s *Store) EffectiveAgentConfigMembers(ctx context.Context, agent AccountID) (version string, members []EffectiveMember, err error)
type EffectiveMember struct { Path string; Layer ConfigLayer }
// go/internal/store/agent_config_merge.go — pure; nil = absent layer.
func MergeConfigBundles(def, tenant, user []byte) (bundle []byte, members []EffectiveMember, err error)
func EffectiveConfigVersion(def, tenant, user string) string // includes configMergeEpoch
```

Tests: each merge-table row; MCP drop only with an overlay; rejected
`profiles/`, oversize, and credential-bearing overlays; write-time effective
check; no overlay → default version and bytes; a put between the two
statements still ships a matching pair; tenant isolation.

### T2 — Proto

`FetchAgentConfigRequest.container_name = 2`; `ConfigVersion.version`
removed (number reserved); `AgentConfigLayer`, three overlay RPCs,
`GetEffectiveAgentConfigInfo`. Regenerate Go and TS.

### T3 — Runnerhub: keyed fetch and nudge

```go
type AgentConfigStore interface {
    EffectiveAgentConfig(ctx context.Context, agent store.AccountID, ifVersion string) (version string, bundle []byte, err error)
}
func (h *Hub) SignalConfigChanged() error
```

Tests: empty / unknown / other-Runner container → InvalidArgument /
PermissionDenied; match → version frame only.

### T4 — Server: overlay RPCs and effective read

Handlers in `go/server/agent_config_service.go`; the four new procedures are
`authenticatedOpen` in `go/internal/auth/admin_gate.go`. Default put/delete
keep their handlers and call `SignalConfigChanged`. Tests: UNSPECIFIED and
DEFAULT rejected; a member writes USER only; an admin writes TENANT; an agent
caller and another user's agent handle are refused.

### T5 — Runner: fetch at Start, cheap refresh

```go
// go/internal/runner/config_materialize.go
type configFetcher interface {
    FetchAgentConfig(ctx context.Context, containerName, ifVersion string) (AgentConfigBundle, error)
}
// mcsLabel runs only after a response that carries chunks.
func (m *ConfigMaterializer) Materialize(ctx context.Context, containerName, ifVersion string, mcsLabel func(context.Context) (string, error)) (ConfigMount, error)
```

Provision calls `ensureRoot` only; Start materializes before exec (podman and
host; vsock skipped; FailedPrecondition degrades with Warn). `maxConfigBytes`
→ 96 MiB. Tests: other fetch errors fail Start; an unchanged container skips
`MountLabel`; a missing matched dir forces a full re-fetch.

### T6 — Agent: settings layers and removal filter

`readMountedSettingsPaths(currentDir): Promise<string[]>` replaces
`readMountedSettingsPath`; `buildFleetSettings` passes the list as
`configFiles`. Before `createAgentSession`, filter `mounted.skills` and
`additionalExtensionPaths` by the merged `skills.ignoredSkills` and
`disabledExtensions`. Tests in `cli.config-passthrough.test.ts`: a user layer
overrides a tenant setting spelled with a legacy key; an ignored default
skill is absent from the session.

### T7 — CLI and docs

`compass agent-config overlay push|delete|info --layer tenant|user` (required)
and `compass agent-config effective <agent-handle>`; default verbs unchanged.
Document layer order, merge table, and removal rules in the agent-config docs.

## Tasks

- [ ] T1 Store: `agent_config_overlays` + RLS, overlay CRUD, merge, one-snapshot resolve
- [ ] T2 Proto: keyed fetch, payload-free signal, overlay and effective RPCs
- [ ] T3 Runnerhub: keyed `FetchAgentConfig`, `SignalConfigChanged`
- [ ] T4 Server: overlay RPCs, layer gating, effective read, nudge on every write
- [ ] T5 Runner: fetch at Start, fetch-before-label refresh, cap raise
- [ ] T6 Agent: per-layer `configFiles`, skills/extensions filter
- [ ] T7 CLI verbs, docs

Order: T2 → T1 → T3/T4 → T5 → T6 → T7. T3 and T5 land in one stack.

## Decisions

- **D1** (DL-495) — Config layers default → tenant → user, resolved on the Server; the
  user is `agent_accounts.owner_user_id`.
- **D2** (DL-496) — Overlays are whole bundles in tenant-owned, FORCE-RLS
  `agent_config_overlays`; NULL `owner_user_id` is the tenant layer.
- **D3** (DL-497) — Members merge per type (§Merge rules); MCP files stay per layer.
- **D4** (DL-498) — Removal: MCP `enabled: false` over the merged result when an
  overlay applies; rules by `ttsr.disabledRules`; skills and extensions by
  an agent-side filter on merged settings.
- **D5** (DL-499) — Settings layer in the SDK: one file per layer as `configFiles`.
- **D6** (DL-500) — Every layer passes the `PutAgentConfig` door; overlays carry no
  `profiles/` and cap at 8 MiB / 512 files; the Runner cap is 96 MiB.
- **D7** (DL-501) — `FetchAgentConfig` requires `container_name`, authorizes like
  `FetchSecrets`, and reads all layers in one statement.
- **D8** (DL-502) — Config materializes at Start on podman and host; vsock skipped.
- **D9** (DL-503) — Effective version: the default version with no overlay, else hex
  sha256 over (`configMergeEpoch`, default, tenant, user).
- **D10** (DL-504) — `SignalConfigChanged` is a payload-free fleet-wide nudge; refresh
  fetches with `if_version` before `MountLabel`.
- **D11** (DL-505) — Overlays get separate `authenticatedOpen` RPCs with a required
  TENANT|USER layer; the default RPCs stay `adminOnly`.
- **D12** (DL-506) — `GetEffectiveAgentConfigInfo` reports each member's winning layer.

## Open Questions

- **OQ-1 (load-bearing) — merge semantics and tenant locks.** Depends on OQ-2.
  - A. Whole-member replace: the highest layer with a member path wins it
    whole, settings and MCP included. Simple and predictable. A user who wants
    one more MCP server or settings key copies the full tenant file, which
    then drifts.
  - B. Per-member merge (D3) plus the removal path OQ-2 picks. Matches how the
    SDK layers settings. A user can replace any tenant member, including
    `prompts/<role>/SYSTEM.md` and `agents/*`; a tenant cannot stop that.
  - C. B plus tenant locks: a tenant-layer `overlay.yml` lists member paths
    and settings keys the user layer may not set; the user door rejects them.
    Adds one manifest member and door logic.
  - **Recommendation: B now.** C adds one member later without changing
    storage or fetch. Pick C now only if tenant policy (for example, locking
    `prompts/`) is a launch requirement.
- **OQ-2 (load-bearing) — removing default skills and extensions.** The SDK
  knobs do nothing for mounted content today.
  - A. Server-side drop: the resolver reads `skills.ignoredSkills` and
    `disabledExtensions` from each settings layer and drops those members. All
    removal in one place (with MCP), but a second settings merge that can
    disagree with the SDK's migrate-then-merge.
  - B. Agent-side filter (D4): compass-agent filters mounted skills and
    extension paths by the SDK-merged settings. One settings authority; the
    knobs also start working for the default layer.
  - C. Add/replace only: skills and extensions cannot be removed; only MCP
    servers and rules can.
  - **Recommendation: B.** D5 already makes the agent the settings authority.
- **OQ-3 (load-bearing) — write-surface shape.** The default is already
  bootstrap-admin-only through `AdminGate`; the question is how overlays avoid
  weakening that.
  - A. Split RPCs (D11): new `authenticatedOpen` overlay RPCs; default RPCs
    untouched. No procedure is reclassified, and no request can fall through
    to the default layer.
  - B. One scoped RPC: `PutAgentConfig` gains a scope and leaves `adminOnly`;
    the bootstrap `adminID` is plumbed into `service` so DEFAULT re-checks
    `caller == admin` in the handler (`service` has no `adminID` today).
  - **Recommendation: A.** The existing gate stays the only guard on the
    default.
- **OQ-4 (load-bearing) — owner axis versus the persona/role seam.**
  - A. Tenant → user (D1). CD-1's persona/role seam later adds a key column
    and a version-tuple slot; this record does not supersede it.
  - B. Tenant → persona/role instead of user, matching CD-1's stated
    direction; no per-user config.
  - C. Tenant → role → user now. Most expressive; needs a role-assignment
    surface this record does not design.
  - **Recommendation: A.** The ask is tenant and user scope; the role layer
    composes later without a fetch or storage redesign.
- **OQ-5 (non-load-bearing) — tenant profiles.** D6 bars `profiles/` in
  overlays; lifting it means the registry guard reads every tenant's overlays.
- **OQ-6 (non-load-bearing) — scoped nudge.** An unchanged container costs one
  version-only round-trip (D10); targeting can wait for fetch metrics.
- **OQ-7 (non-load-bearing) — MCP header literals.** The door does not lint
  MCP `headers` (pre-existing, default too); reuse `isEnvIndirection` later.
