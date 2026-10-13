# Compass agent config overlays — tenant, user, and role layers

Refs: RIG-3112

> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../../agent/compass-agent-config-delivery/design.md` header).

**Named decisions this record introduces:** D1–D14 in
[Decisions](#decisions).

## Resolved decisions

Ruled (Matt, 2026-10-13):

- **OQ-1** — whole-member replace plus tenant locks (D3); per-member merge is
  out.
- **OQ-2** — agent-side removal filter (D4).
- **OQ-3** — split overlay RPCs (D11).
- **OQ-4** — default → tenant → user → role (D1).
- **OQ-5** — field lint (D13); CD-3's credential MUST now covers only D13's
  fields.

## Open Questions

- **OQ-6 (non-load-bearing) — tenant profiles.** D6 bars `profiles/` in
  overlays; lifting it means the registry guard reads every tenant's overlays.
- **OQ-7 (non-load-bearing) — scoped nudge.** An unchanged container costs one
  version-only round-trip (D10); targeting can wait for fetch metrics.

## Problem / Intent

`agent_config_bundle` is a singleton, so every agent in every tenant gets the
same config. This record keeps it as the **default** layer, adds **tenant**,
**user**, and **role** overlays with tenant locks, and has the Server resolve
one effective bundle per agent.

## Approach

### Ground truth

- **Default layer.** `agent_config_bundle` (`0001_init.sql`) is a
  credential-free singleton with no `tenant_id`, RLS-exempt (`bucketA` in
  `go/internal/store/rls_pgtest_test.go`).
- **Writers and roles.** `PutAgentConfig`/`DeleteAgentConfig` are `adminOnly`
  (`go/internal/auth/admin_gate.go`); only `BootstrapAdmin` writes
  `UserRoleAdmin` (`go/internal/store/accounts.go`).
- **User coordinate.** `agent_accounts.owner_user_id` is set at insert and
  never updated; `SecretRecordsForAgent` already layers on it
  (`s.scope_kind = 1 AND s.scope_id = a.owner_user_id`).
- **Role coordinate.** `agent_accounts.role` (`0001_init.sql`: "the
  operator-set block-0 selector") is set at creation and never updated; the
  only `agent_accounts` update is `UpdateAgentParent`. Request paths check it
  with `IsManagerRole` (`supervisor`, `owner`, `manager`); older rows may hold
  an empty or unknown label (`AgentAccount.Role`, `go/internal/store/types.go`).

### Design

**Layers and order (D1).** Effective bundle = default → tenant → user → role,
most specific last; a missing layer is skipped, and with no overlay it is the
default bytes. The role layer keys on the agent's `role`, so an agent with an
empty role gets none, and an unknown legacy label matches no row. Assumption,
pending Matt's confirmation: a role layer is per user, keyed (tenant, owner,
role), not per tenant. It is CD-1's named role-keyed seam; nothing keys on an
agent id.

**Storage (D2).** One tenant-owned table holds whole-bundle overlays, shaped
like `account_handles` (nullable owner FK plus two partial unique indexes):

```sql
CREATE TABLE agent_config_overlays (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
                  REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_user_id TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT, -- NULL = tenant layer
    role          TEXT NOT NULL DEFAULT '', -- '' = tenant or user layer; else that user's role layer
    version       TEXT NOT NULL,
    bundle        BYTEA NOT NULL,
    declared_by   TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_config_overlays_role_owner CHECK (role = '' OR owner_user_id IS NOT NULL)
);
CREATE UNIQUE INDEX agent_config_overlays_tenant_key ON agent_config_overlays (tenant_id) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX agent_config_overlays_owner_key ON agent_config_overlays (tenant_id, owner_user_id, role) WHERE owner_user_id IS NOT NULL;
```

A user or role write sets `owner_user_id = caller`, so the owner is in the
writer's tenant; a role write also sets `role`, which must pass
`IsManagerRole`.

**Merge rules (D3).** The highest layer with a member wins it whole; nothing
merges inside a member.

| Member | Rule |
|---|---|
| Any file: `settings/config.yml`, `models.yml`, `AGENTS.md`, `mcp/<server>.json`, `rules/*`, `agents/*`, `prompts/<role>/SYSTEM.md` | Whole file by path from the highest layer that has it. |
| `skills/<name>/`, `extensions/<id>/` | Whole directory from the highest layer that has it; files never mix across layers. `extensions/<id>/` is the only extension form (below). |
| `profiles/` | Default layer only (D6). |
| `locks.yml` | Tenant layer only; the Server reads it and never ships it. |

**Extension identity (D3).** Extensions are directories only: the store door
(`validateRegularMember`, `go/internal/store/agent_config.go`), Runner
(`validateNestedRegularMember`), and CLI (`validateMemberGrammar`) already
reject a dotted `extensions/foo.ts` by `^[A-Za-z0-9_-]+$`; the door also
rejects a file at `extensions/<id>`. A `package.json` can still rename:
`resolveExtensionEntries` loads each `omp.extensions` (else `pi.extensions`)
path, which `getExtensionNameFromPath` names by basename (parent directory for
`index.*`). The door requires each entry under `extensions/<id>/` to derive
`<id>`, so merge, removal, and SDK ids agree.

**MCP identity (D3).** `readMountedMcpConfigs`
(`packages/compass-agent/src/config-reader.ts`) reads files in sorted name
order and does `configs[serverName] = config`, so two files naming one server
collide by file name, not layer. The door makes path identity server
identity: `validateMCPMember` requires `mcp/<server>.json` to hold an
`mcpServers` object whose only key is `<server>`, or no key. Whole-file
replace by path then gives each server layer precedence. The repo's shipped
`config/` has no `mcp/`; any default MCP file comes from an operator's
`agent-config push --dir`. A stored default with another shape fails the D14
boot lint, so T6's docs tell operators to split it, one file per server,
before upgrading.

**Tenant locks (D3).** The tenant's `locks.yml` lists `paths:`, each a member
path or a `/`-ended prefix (`prompts/`, `skills/web/`); an entry inside a
directory member is rejected. A user or role put reads the tenant row's locks
in its transaction and rejects a covered member. A later lock never breaks a
stored row: the merge drops the covered user or role member, and the resolver
logs a Warn naming layer and path. Locks are path-level; locking
`mcp/<server>.json` locks that server.

**Removal (D4).** Overlays only add or replace; removal is:

- **MCP.** `mcp/<server>.json` with an empty `mcpServers` drops `<server>`;
  `MCPManager.connectServers` ignores `enabled`.
- **Rules.** `ttsr.disabledRules` in the winning `settings/config.yml` (the
  SDK applies it).
- **Skills and extensions.** `cli.ts` passes `skills: mounted.skills` and
  `disableExtensionDiscovery: true`, so `createAgentSession` (`sdk.ts`) skips
  both knobs. compass-agent filters first by the winning settings file,
  matching the SDK:
  `skills.includeSkills`/`skills.ignoredSkills` match names by
  `new Bun.Glob(pattern)`, `skill:<name>` in `disabledExtensions` drops a
  skill (`extensibility/skills.ts`), and `extension-module:<id>` drops an entry
  whose `getExtensionNameFromPath` is `<id>` (`discoverExtensionPaths`).

**Settings (D5).** `settings/config.yml` replaces whole like any file, so the
agent loads one settings file through the unchanged `buildFleetSettings`
(`packages/compass-agent/src/cli.ts`). To change one key, a user copies the
winning file; a tenant lock on it fixes the tenant's settings.

**One door, one effective cap (D6).** Every layer write runs the
`validateAndHashConfigBundle` door for its layer: only the default carries
`profiles/`, so `PutAgentConfig`'s orphan guard stays default-only, and only
the tenant carries `locks.yml`. A 48 MiB / 2560-file default and three
4 MiB / 512-file overlays fit the door's 64 MiB / 4096
(`maxDecompressedBytes`, `maxFileCount`) and the Runner's `maxConfigBytes`
without a cross-layer read; whole-member replace adds no files or bytes.

**Cross-layer checks (D14).** Per-value lints hold for merged clean
layers. `lintProfileAgentKeys`, the only cross-layer lint, runs on the
default, so an overlay `agents/x.md` orphaning a profile key gets a resolver
Warn. No stored default is grandfathered: each boot runs
`LintStoredAgentConfig` (T5) before mounting doors, and a hit fails startup
and the overlay rollout. The fetch re-checks only the cap and member paths,
failing closed with `CodeInternal`
(`effective config for agent %s invalid (default %s, tenant %s, user %s, role %s): %v`).

**Keyed fetch, one snapshot (D7).** `FetchAgentConfigRequest`
(`proto/compass/v1/runner.proto`) gains a required `container_name = 2`; the
unkeyed fetch is deleted. `Handler.FetchAgentConfig` mirrors `FetchSecrets`:
`AccountForContainer` → `accountTenantContext`
(`go/internal/runnerhub/bind_lifetime.go`) →
`EffectiveAgentConfig(ctx, agent, ifVersion)`. `beginTenantTx` is READ
COMMITTED, so one statement returns all layer rows:

```sql
SELECT 1 AS layer, version, bundle FROM agent_config_bundle WHERE singleton
UNION ALL
SELECT CASE WHEN o.owner_user_id IS NULL THEN 2 WHEN o.role = '' THEN 3 ELSE 4 END, o.version, o.bundle
  FROM agent_config_overlays o JOIN agent_accounts a ON a.account_id = $1
 WHERE o.owner_user_id IS NULL
    OR (o.owner_user_id = a.owner_user_id AND o.role IN ('', a.role));
```

A version-only variant answers `if_version`; a miss recomputes the version
from the full rows. The fetch needs the binding `Hub.enroll` clears; Runner
`Run` (`go/internal/runner/run.go`) sweeps stale containers each dial, and
`lifecycleService.reprovision` (`go/server/lifecycle.go`) binds a re-created
container before Start.

**Materialize at Start (D8).** `Hub.Provision` binds the container after the
relay returns, so `Host.Provision` (`go/internal/runner/host.go`) cannot
fetch keyed and only runs `ensureRoot`. Start fetches with `if_version`,
materializes before exec next to `FetchSecretsByContainer`, and records
`h.configVersions[name]`. Podman uses the live `MountLabel`; vsock is skipped.
The `CodeFailedPrecondition` "no config surface" degrade stays; other fetch
errors fail Start.

**Effective version (D9).** With no overlay row, the default version; else
lowercase hex sha256 (a `ConfigMaterializer` path segment) over the
length-framed tuple (`configMergeEpoch`, default, tenant, user, role; `""` when
absent). A matched version with no `root/<version>` forces a re-fetch. An
epoch bump sends no signal; containers see it at the next Start or nudge.

**Refresh signal (D10).** `SignalConfigVersion(version)` becomes
`SignalConfigChanged()`, a payload-free fleet-wide nudge sent by every layer's
put and delete. `refreshOneContainer` fetches with the tracked version
before `MountLabel`; a version-only response skips inspect, relabel, and
Reload.

**Write and read surface (D11, D12).** The default RPCs stay
`adminOnly`; new `authenticatedOpen` RPCs carry overlays:

```proto
enum AgentConfigLayer {
  AGENT_CONFIG_LAYER_UNSPECIFIED = 0;
  AGENT_CONFIG_LAYER_DEFAULT = 1;  // reported by the effective read only
  AGENT_CONFIG_LAYER_TENANT = 2;
  AGENT_CONFIG_LAYER_USER = 3;
  AGENT_CONFIG_LAYER_ROLE = 4;
}
rpc PutAgentConfigOverlay(PutAgentConfigOverlayRequest) returns (PutAgentConfigOverlayResponse);
rpc DeleteAgentConfigOverlay(DeleteAgentConfigOverlayRequest) returns (DeleteAgentConfigOverlayResponse);
rpc GetAgentConfigOverlayInfo(GetAgentConfigOverlayInfoRequest) returns (GetAgentConfigInfoResponse);
rpc GetEffectiveAgentConfigInfo(GetEffectiveAgentConfigInfoRequest) returns (GetEffectiveAgentConfigInfoResponse);
message PutAgentConfigOverlayRequest { AgentConfigLayer layer = 1; bytes bundle = 2; string role = 3; }  // role: ROLE only
message PutAgentConfigOverlayResponse { string version = 1; }  // the layer's own version
message DeleteAgentConfigOverlayRequest { AgentConfigLayer layer = 1; string role = 2; }
message DeleteAgentConfigOverlayResponse {}
message GetAgentConfigOverlayInfoRequest { AgentConfigLayer layer = 1; string role = 2; }
message GetEffectiveAgentConfigInfoRequest { string agent_handle = 1; }
message EffectiveConfigMember { string path = 1; AgentConfigLayer layer = 2; }  // winning layer
message GetEffectiveAgentConfigInfoResponse { string version = 1; repeated EffectiveConfigMember members = 2; }  // D9 version; members sorted by path
```

Overlay RPCs accept TENANT, USER, or ROLE (else `InvalidArgument`); `role` is
required for ROLE and empty otherwise. USER and ROLE address the caller's own
rows; agents have no write door.

| RPC | TENANT | USER | ROLE |
|---|---|---|---|
| Put / Delete | `UserRoleAdmin`, checked after `requireUser` as the user-secret store does for `SecretScope` | caller | caller, for a role passing `IsManagerRole` |
| Overlay info (names only, as `GetAgentConfigInfo`) | any user in the tenant, since all their agents consume it | caller | caller |
| Effective info | the caller's own agent, via `AgentByHandle(ctx, caller, handle)` | same | same |

Granting `UserRoleAdmin` beyond the bootstrap tenant is out of scope.

**MCP credential lint (D13).** CD-3 says "an MCP config MUST
NOT embed a credential"; `validateMCPMember` checks only JSON. As ruled, that
MUST now covers what `rejectCredentialMCP` rejects, per server, on every layer:

- `oauth.clientSecret` and `auth.clientSecret` (`MCPServerConfigBase`, SDK
  `src/mcp/types.ts`);
- `envPolicy` and `headerPolicy`, which make every `env` or `headers` value a
  literal (`MCPManager.#resolveAuthConfig`);
- in `url`, `auth.tokenUrl` (`refreshManagedMcpOAuthCredential` hands it to
  `refreshMCPOAuthToken`, which fetches it), or a URL-shaped `args` element,
  userinfo or a query key matching `SENSITIVE_QUERY_PARAM`
  (`/key|token|secret|auth/i`, `src/mcp/json-rpc.ts`);
- an `args` element matching `EXA_API_KEY_PATTERN` (`/exaApiKey=([^&\s]+)/i`),
  which `extractExaApiKey` (`src/mcp/config.ts`) reads from `url`, `args`,
  and `env`;
- an `env` or `headers` value that is not an indirection.

An indirection is exactly what `isEnvIndirection` admits: a leading `!`, or a
value matching `^[A-Za-z_][A-Za-z0-9_]*$`. That is shape, not proof:
`resolveConfigValue` (`src/config/resolve-config-value.ts`) "checks
environment variable first, then treats as literal", so an identifier-shaped
token, a `!command` that prints one, or a token in `command` or another `args`
element passes; the writer owns that residual. `PORT: "8080"` needs the
`!command` form.

**Trust boundary.** A user layer reaches only its user's agents, and a role
layer only those with its role; a tenant layer reaches only its tenant, and
only tenant admins write it and its locks. Code-bearing members (extensions,
MCP `!command` values) run only in agents the layer reaches.

## Alternatives considered

- **Member-granular overlay rows.** Rejected: per-member API and validator;
  no atomic layer push.
- **Per-member merge** (settings deep-merge, MCP by server name, `AGENTS.md`
  concatenation). Rejected by Matt for whole-member replace plus locks.
- **Merge in the agent.** Rejected: a Server-side merge gives one reportable
  tree (D12).
- **Revalidate dependent layers per write.** Rejected (D14): a default write
  would read every tenant's overlays.
- **Pending binding at Provision.** Rejected: a second binding lifecycle.

## Plan

### Global Constraints

- Public repo: no private-product detail
  (`docs/designs/meta/oss-core-managed-boundary/design.md`).
- Schema phase: `runMigrationCheck` (`tools/sql-migration-gate/index.ts`) is
  the switch. While it is the stub ("Suspended while open migration PRs fold
  into 0001_init.sql"), edit `0001_init.sql` in place: the table before the
  schema-wide grant, named in `tenant_tables` and `updated_at_tables` (as
  `docs/designs/server/compass-workstream-repository-source/design.md` does).
  Once it calls `checkMigrationImmutability`, add the next numbered migration
  with its own grants, forced `tenant_isolation` policy, and `set_updated_at`
  trigger. Either way, list the table in `tenantOwned` (`rls_pgtest_test.go`).
- RLS: layer reads run as `compass_app` under the tenant GUC; the fetch's
  only system-role step is the `AccountTenant` lookup `FetchSecrets` uses.
- Clean cutover: no unkeyed `FetchAgentConfig`, `SignalConfigVersion`, or
  Provision-time fetch remains; migrate fakes and test clients
  (`go/internal/runnerhub/config_fetch_test.go`,
  `go/server/runner_config_compose_pgtest_test.go`).
- One linear stack, T1 → T6; each PR passes alone, red → green.

### T1 — Store: schema, lints, merge, resolve

```go
// go/internal/store/agent_config_overlay.go — tenant from ctx.
type OverlayKey struct { Owner AccountID; Role string } // {} tenant; {Owner} user; {Owner, Role} role
// A user or role put reads the tenant row's locks in its transaction; Role must pass IsManagerRole.
func (s *Store) PutAgentConfigOverlay(ctx context.Context, actor AccountID, key OverlayKey, bundle []byte) (version string, err error)
func (s *Store) DeleteAgentConfigOverlay(ctx context.Context, key OverlayKey) error
func (s *Store) AgentConfigOverlayInfo(ctx context.Context, key OverlayKey) (AgentConfigInfoResult, error)
// bundle nil on an ifVersion match; else the one-statement full read. One Warn per dropped member.
func (s *Store) EffectiveAgentConfig(ctx context.Context, agent AccountID, ifVersion string) (version string, bundle []byte, err error)
func (s *Store) EffectiveAgentConfigMembers(ctx context.Context, agent AccountID) (version string, members []EffectiveMember, err error)
type ConfigLayer int8 // LayerDefault = 1, LayerTenant, LayerUser, LayerRole: the AgentConfigLayer values
type EffectiveMember struct { Path string; Layer ConfigLayer }
// go/internal/store/agent_config_merge.go — pure. A member is a file, or a whole skills/<name>/ or
// extensions/<id>/ directory; the highest layer with it wins. locks.yml never ships; dropped lists
// the user and role members the tenant's locks cover.
type LayerBundles struct { Default, Tenant, User, Role []byte } // nil = absent layer
func MergeConfigBundles(in LayerBundles) (bundle []byte, members, dropped []EffectiveMember, err error)
func EffectiveConfigVersion(def, tenant, user, role string) string // includes configMergeEpoch
// go/internal/store/agent_config_locks.go — locks.yml is a mapping with only `paths:`, a list of
// member paths or "/"-ended prefixes; an entry inside a directory member fails.
type ConfigLocks struct { paths []string }
func parseConfigLocks(body []byte) (ConfigLocks, error)
func (l ConfigLocks) Covers(member string) bool // exact match, or under a "/"-ended prefix
// go/internal/store/agent_config.go — every layer's door. The layer picks the D6 cap and admits
// profiles/ (default only) and locks.yml (tenant only). maxDecompressedBytes and maxFileCount
// stay as the effective-bundle cap the fetch re-checks (D14).
type layerCaps struct { bytes int64; files int }
var defaultLayerCaps, overlayLayerCaps = layerCaps{48 << 20, 2560}, layerCaps{4 << 20, 512}
func validateAndHashConfigBundle(bundle []byte, layer ConfigLayer) (version string, profiles map[string][]byte, locks ConfigLocks, err error)
// PutAgentConfig, ValidateConfigBundle, and LintStoredAgentConfig pass LayerDefault.
// validateMCPMember (D3): mcp/<server>.json's mcpServers object has no key or only <server>.
func rejectCredentialMCP(doc map[string]any, joined string) error
// After the member stream. manifests: extensions/<id>/package.json bodies keyed by <id>;
// members: every regular member path. Rejects a file at extensions/<id>; each omp (else pi)
// .extensions entry must be a member under extensions/<id>/ whose extensionIDFromPath is <id>
// (D3). An unparseable package.json is skipped, as the SDK's readExtensionManifest skips it.
func lintExtensionManifests(manifests map[string][]byte, members map[string]bool) error
func extensionIDFromPath(p string) string // ports the SDK's getExtensionNameFromPath
```

Tests: a user `settings/config.yml` drops a tenant-only key; a user
`skills/x/` keeps none of the default's `skills/x/` files; a two-server
`mcp/a.json`, an `mcp/a.json` keyed `b`, and one with no `mcpServers` rejected;
a user `mcp/fetch.json` overrides the default's, and one with empty
`mcpServers` removes `fetch`; under `extensions/foo/`, `src/index.ts` (id
`src`) and `../bar/index.ts` rejected, `foo.ts` accepted; a file at
`extensions/foo` rejected; each D13 carrier rejected, `auth.tokenUrl` userinfo
and `?token=` included; an env name and `!command` accepted; a user or role put
under an exact or prefix lock rejected; a lock added after a put drops that
member with a Warn; `locks.yml` outside the tenant layer, and a lock inside
`skills/x/`, rejected; an empty role gets no role layer, and another user's or
role's row never applies; an off-taxonomy role rejected; four max-size layers
fit the effective cap; `profiles/` and oversize layers rejected; an orphan
profile key warns; a row written around the door fails closed; no overlay →
default bytes; a put between the two statements ships a matching pair; tenant
isolation. T1 migrates door-crossing MCP fixtures (`{"url":"https://x"}`,
`{"a":1}`, `{"ok":true}`) in the store, server, and `TestBuildBundleDoorParity`
tests to the D3 shape.

### T2 — Signal cutover

Remove `ConfigVersion.version` (number reserved) and its call sites:
`Hub.SignalConfigChanged() error` replaces `SignalConfigVersion`
(`go/internal/runnerhub/config_signal.go`), plus the `signaler` interface,
`signalConfigVersion` (`go/server/agent_config_service.go`), and the
`GetVersion()` log field (`go/internal/runner/dispatch.go`). Tests: a nudge
reaches each attached router; no-op with no live sessions.

### T3 — Keyed fetch and Start materialize

One PR: the handler rejects an unkeyed request.

```go
type AgentConfigStore interface { // go/internal/runnerhub/handler.go
    EffectiveAgentConfig(ctx context.Context, agent store.AccountID, ifVersion string) (version string, bundle []byte, err error)
}
type configFetcher interface { // go/internal/runner/config_materialize.go
    FetchAgentConfig(ctx context.Context, containerName, ifVersion string) (AgentConfigBundle, error)
}
// mcsLabel runs only after a response that carries chunks.
func (m *ConfigMaterializer) Materialize(ctx context.Context, containerName, ifVersion string, mcsLabel func(context.Context) (string, error)) (ConfigMount, error)
```

Runner per D8. Tests: an empty, unknown, or other-Runner container rejected; a
match returns only the version; other fetch errors fail Start; an unchanged
nudge skips `MountLabel` and Reload; a missing matched dir re-fetches.

### T4 — Agent: removal filter

```ts
// packages/compass-agent/src/config-reader.ts — D4 over skills and additionalExtensionPaths.
export function filterMountedConfig(mounted: MountedConfig, settings: Settings | undefined): MountedConfig
```

`main` calls it with `fleetSettings` before `createAgentSession`. Tests
(`cli.config-passthrough.test.ts`): `includeSkills: ["web-*"]` keeps
`web-search` and drops `notes`; `ignoredSkills: ["web-*"]` drops
`web-search`; `disabledExtensions: ["skill:foo"]` drops skill `foo`;
`disabledExtensions: ["extension-module:bar"]` drops `bar` and keeps `baz`;
no settings file filters nothing.

### T5 — Server: overlay RPCs and effective read

Handlers in `go/server/agent_config_service.go`, all four `authenticatedOpen`;
every put and delete, default included, calls `SignalConfigChanged`. The boot
lint (D14):

```go
// go/internal/store/agent_config.go: validateAndHashConfigBundle(bundle, LayerDefault) over
// CurrentAgentConfig; ErrNotFound → nil. Serve (go/server/serve.go) calls it right after
// seedBootstrapAccounts and passes a hit to failStartup.
func (s *Store) LintStoredAgentConfig(ctx context.Context) error
```

Tests: each authorization-table row; UNSPECIFIED and DEFAULT, ROLE with an
empty or off-taxonomy role, and TENANT or USER with a role rejected; an agent
caller denied; the effective read names winning layers and refuses another
user's agent; `Serve` fails on a stored default with a literal MCP `env` value,
a two-server MCP file, or 2561 files, and starts on an absent or clean one.

### T6 — CLI and docs

`compass agent-config overlay push|delete|info --layer tenant|user|role`
(required, with `--role <role>` for `role`) and
`compass agent-config effective <agent-handle>`; default verbs unchanged.
`maxBundleFileCount` and `maxBundleContentBytes` become the D6 layer caps,
only the tenant layer admits `locks.yml`, and `validateMemberGrammar` mirrors
the D3 MCP shape:

```go
// go/cmd/compass/bundle.go
type bundleLayer struct { files int; contentBytes int64; locks bool }
var (
    defaultBundleLayer = bundleLayer{files: 2560, contentBytes: 48 << 20}
    tenantBundleLayer  = bundleLayer{files: 512, contentBytes: 4 << 20, locks: true}
    overlayBundleLayer = bundleLayer{files: 512, contentBytes: 4 << 20} // user and role
)
func buildBundle(dir string, layer bundleLayer) ([]byte, error)
func checkBundleCaps(layer bundleLayer, fileCount int, contentBytes int64) error
```

Tests: a 2561-file default, a 513-file overlay, a 4 MiB + 1-byte overlay,
`locks.yml` in a user dir, and a two-server MCP file fail, naming the cause.
Docs: layer order, merge table, locks, removal rules, MCP lint, and splitting a
multi-server MCP file.

## Tasks

- [ ] T1 Store
- [ ] T2 Signal cutover
- [ ] T3 Keyed fetch and Start materialize
- [ ] T4 Agent removal filter
- [ ] T5 Server RPCs
- [ ] T6 CLI and docs

## Decisions

- **D1** (DL-495) — Config layers default → tenant → user → role, resolved on
  the Server; the user is `agent_accounts.owner_user_id`, and the role layer,
  keyed (tenant, owner, `agent_accounts.role`), applies only to an agent with a
  non-empty role.
- **D2** (DL-496) — Overlays are whole bundles in tenant-owned, FORCE-RLS
  `agent_config_overlays`; NULL `owner_user_id` is the tenant layer, and an
  owner with a non-empty `role` is that user's role layer.
- **D3** (DL-497) — The highest layer with a member wins it whole: a file, or
  a whole `skills/<name>/` or `extensions/<id>/` directory; nothing merges
  inside a member. An MCP file is `mcp/<server>.json` whose `mcpServers` holds
  only `<server>` or nothing, so path identity is server identity. The
  tenant's `locks.yml` lists paths and `/`-ended prefixes the user and role
  layers may not set: their door rejects a covered member, and the merge drops
  a stored one with a Warn. The door requires each extension manifest entry's
  SDK-derived id to equal `<id>`.
- **D4** (DL-498) — Removal: an MCP server by its `mcp/<server>.json` with an
  empty `mcpServers`; rules by `ttsr.disabledRules`; skills and extensions by
  an agent-side filter that reads the winning `settings/config.yml` and
  reproduces the SDK's matching rules.
- **D5** (DL-499) — Settings are whole-file: the winning `settings/config.yml`
  is the agent's only settings file, loaded by the unchanged
  `buildFleetSettings`.
- **D6** (DL-500) — Every layer passes the `PutAgentConfig` door; only the
  default carries `profiles/` and only the tenant `locks.yml`; layer caps
  (default 48 MiB / 2560 files, overlay 4 MiB / 512) keep every four-layer
  effective bundle within one 64 MiB / 4096-file cap.
- **D7** (DL-501) — `FetchAgentConfig` requires `container_name`, authorizes
  like `FetchSecrets`, and reads all layers in one statement.
- **D8** (DL-502) — Config materializes at Start on podman and host, and
  Start records the container's version; vsock skipped.
- **D9** (DL-503) — Effective version: the default version with no overlay,
  else hex sha256 over (`configMergeEpoch`, default, tenant, user, role).
- **D10** (DL-504) — `SignalConfigChanged` is a payload-free fleet-wide nudge;
  refresh fetches with `if_version` before `MountLabel`.
- **D11** (DL-505) — Overlays get separate `authenticatedOpen` RPCs with a
  required TENANT|USER|ROLE layer, ROLE naming a taxonomy role; the default
  RPCs stay `adminOnly`.
- **D12** (DL-506) — `GetEffectiveAgentConfigInfo` reports each member's
  winning layer.
- **D13** (DL-511) — The door rejects, on every layer, MCP
  `oauth.clientSecret`, `auth.clientSecret`, `envPolicy`, `headerPolicy`, URL
  userinfo and secret-named query keys in `url`, `auth.tokenUrl`, and
  URL-shaped `args`, `exaApiKey=` in `args`, and any `env` or `headers` value
  that is neither a `!command` nor an env name; CD-3's MUST is revised to
  these fields.
- **D14** (DL-512) — Each boot lints the stored default with the default door
  before mounting doors and fails startup on a hit; the fetch re-checks only
  the Runner's rules, failing closed with a named error; an orphaned profile
  key on the effective bundle logs a Warn and delivers.
