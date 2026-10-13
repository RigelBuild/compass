# Compass agent config overlays — tenant and user layers

Refs: RIG-3112

> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../../agent/compass-agent-config-delivery/design.md` header).

**Named decisions this record introduces:** D1–D14 in
[Decisions](#decisions).

## Problem / Intent

`agent_config_bundle` is a singleton, so every agent in every tenant gets the
same config. This record keeps it as the **default** layer, adds **tenant**
and **user** overlays, and has the Server resolve one effective bundle per
agent.

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

### Design

**Layers and order (D1, assumes OQ-4 A).** Effective bundle =
default → tenant → user; a missing layer is skipped, and with no overlay it is
the default bytes. A user is not an agent id, so CD-1's seam ("never an
individual agent id") stays open (OQ-4).

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

A user write sets `owner_user_id = caller`, so the owner is in the writer's
tenant.

**Merge rules (D3, assumes OQ-1 B).**

| Member | Rule |
|---|---|
| `settings/config.yml` | Not merged on the Server. Each layer's file is emitted as `settings/config.yml`, `settings/tenant.yml`, `settings/user.yml` (D5). |
| `models.yml` | YAML deep-merge on the Server: objects recurse, arrays and scalars replace. The SDK reads one `models.yml` path, so it cannot layer it. No key normalization. |
| `mcp/*.json` | Kept per layer as `mcp/<layer>-<name>.json` (`default-` < `tenant-` < `user-` in sort order), so `readMountedMcpConfigs` merges by server name, later wins, and keeps per-file provenance. Drops follow D4. |
| `AGENTS.md` | Concatenate default, tenant, user with a blank line between. |
| `skills/<name>/` | Whole directory from the highest layer that has it. |
| `extensions/<id>/` | Whole directory from the highest layer that has it; the only extension form (below). |
| `rules/*`, `agents/*`, `prompts/*` | Whole file by path from the highest layer that has it. |
| `profiles/` | Default layer only (D6). |

**Extension identity (D3).** Extensions are directories only: the store door
(`validateRegularMember`, `go/internal/store/agent_config.go`), Runner
(`validateNestedRegularMember`), and CLI (`validateMemberGrammar`) already
reject a dotted `extensions/foo.ts` by `^[A-Za-z0-9_-]+$`; the door also
rejects a file at `extensions/<id>`. A `package.json` can still rename:
`resolveExtensionEntries` loads each `omp.extensions` (else `pi.extensions`)
path, which `getExtensionNameFromPath` names by basename (parent directory for
`index.*`). The door requires each entry under `extensions/<id>/` to derive
`<id>`, so merge, removal, and SDK ids agree.

**Removal (D4, assumes OQ-2 B).** Overlays only add or replace; removal is:

- **MCP.** With an overlay, the resolver drops each server whose winning entry
  has `"enabled": false` from every layer file, since
  `MCPManager.connectServers` ignores `enabled`; with none, nothing changes.
- **Rules.** `ttsr.disabledRules` in any settings layer (the SDK applies it).
- **Skills and extensions.** `cli.ts` passes `skills: mounted.skills` and
  `disableExtensionDiscovery: true`, so `createAgentSession` (`sdk.ts`) skips
  both knobs. compass-agent filters first, matching the SDK:
  `skills.includeSkills`/`skills.ignoredSkills` match names by
  `new Bun.Glob(pattern)`, `skill:<name>` in `disabledExtensions` drops a
  skill (`extensibility/skills.ts`), and `extension-module:<id>` drops an entry
  whose `getExtensionNameFromPath` is `<id>` (`discoverExtensionPaths`).

**Settings layered by the SDK (D5).** `#loadOverlayYaml` (`settings.ts`) runs
`#migrateRawSettings` per file before `#deepMerge`, so a Server-side merge
could invert layer order across legacy and current keys. `buildFleetSettings`
(`packages/compass-agent/src/cli.ts`) passes each layer's file, in order, as
`configFiles`; the door still accepts only `settings/config.yml`.

**One door, one effective cap (D6).** Every layer write runs the
`validateAndHashConfigBundle` door at its layer's cap; overlays carry no
`profiles/`, so `PutAgentConfig`'s orphan guard stays default-only. A
48 MiB / 3000-file default and two 4 MiB / 512-file overlays fit the door's
64 MiB / 4096 (`maxDecompressedBytes`, `maxFileCount`) and the Runner's
`maxConfigBytes` without a cross-layer read. The merge adds no files; its
added bytes (`AGENTS.md` separators, re-encoded `models.yml`) count.

**Cross-layer checks (D14).** Per-value lints hold for merged clean
layers. `lintProfileAgentKeys`, the only cross-layer lint, runs on the
default, so an overlay `agents/x.md` orphaning a profile key gets a resolver
Warn. No stored default is grandfathered: each boot runs
`LintStoredAgentConfig` (T5) before mounting doors, and a hit fails startup
and the overlay rollout. The fetch re-checks only the cap and member paths,
failing closed with `CodeInternal`
(`effective config for agent %s invalid (default %s, tenant %s, user %s): %v`).

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
SELECT CASE WHEN o.owner_user_id IS NULL THEN 2 ELSE 3 END, o.version, o.bundle
  FROM agent_config_overlays o JOIN agent_accounts a ON a.account_id = $1
 WHERE o.owner_user_id IS NULL OR o.owner_user_id = a.owner_user_id;
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
length-framed tuple (`configMergeEpoch`, default, tenant, user; `""` when
absent). A matched version with no `root/<version>` forces a re-fetch. An
epoch bump sends no signal; containers see it at the next Start or nudge.

**Refresh signal (D10).** `SignalConfigVersion(version)` becomes
`SignalConfigChanged()`, a payload-free fleet-wide nudge sent by every layer's
put and delete. `refreshOneContainer` fetches with the tracked version
before `MountLabel`; a version-only response skips inspect, relabel, and
Reload.

**Write and read surface (D11, D12, assumes OQ-3 A).** The default RPCs stay
`adminOnly`; new `authenticatedOpen` RPCs carry overlays:

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
rpc GetEffectiveAgentConfigInfo(GetEffectiveAgentConfigInfoRequest) returns (GetEffectiveAgentConfigInfoResponse);
message PutAgentConfigOverlayRequest { AgentConfigLayer layer = 1; bytes bundle = 2; }
message PutAgentConfigOverlayResponse { string version = 1; }  // the layer's own version
message DeleteAgentConfigOverlayRequest { AgentConfigLayer layer = 1; }
message DeleteAgentConfigOverlayResponse {}
message GetAgentConfigOverlayInfoRequest { AgentConfigLayer layer = 1; }
message GetEffectiveAgentConfigInfoRequest { string agent_handle = 1; }
message EffectiveConfigMember { string path = 1; AgentConfigLayer layer = 2; }  // winning layer
message GetEffectiveAgentConfigInfoResponse { string version = 1; repeated EffectiveConfigMember members = 2; }  // D9 version; members sorted by path
```

Overlay RPCs accept only TENANT or USER (else `InvalidArgument`); USER is
always the caller's own layer; agents have no write door.

| RPC | TENANT | USER |
|---|---|---|
| Put / Delete | `UserRoleAdmin`, checked after `requireUser` as the user-secret store does for `SecretScope` | caller |
| Overlay info (names only, as `GetAgentConfigInfo`) | any user in the tenant, since all their agents consume it | caller |
| Effective info | the caller's own agent, via `AgentByHandle(ctx, caller, handle)` | same |

Granting `UserRoleAdmin` beyond the bootstrap tenant is out of scope.

**MCP credential lint (D13, assumes OQ-5 A).** CD-3 says "an MCP config MUST
NOT embed a credential"; `validateMCPMember` checks only JSON. D13 narrows
that MUST to what `rejectCredentialMCP` rejects, per server, on every layer:

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
token or a `!command` that prints one passes (OQ-5). `PORT: "8080"` needs the
`!command` form.

**Trust boundary.** A user layer reaches only its user's agents; a tenant
layer, only its tenant, and only tenant admins write it. Code-bearing
members (extensions, MCP `!command` values) run only for the writer's agents.

## Alternatives considered

- **Member-granular overlay rows.** Rejected: per-member API and validator;
  no atomic layer push.
- **Merge in the agent.** Rejected: a Server-side merge gives one reportable
  tree (D12); D5 keeps settings hybrid.
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
// go/internal/store/agent_config.go — every layer's door, capped per D6. maxDecompressedBytes
// and maxFileCount stay as the effective-bundle cap the fetch re-checks (D14).
type layerCaps struct { bytes int64; files int }
var defaultLayerCaps, overlayLayerCaps = layerCaps{48 << 20, 3000}, layerCaps{4 << 20, 512}
func validateAndHashConfigBundle(bundle []byte, caps layerCaps) (version string, profiles map[string][]byte, err error)
// PutAgentConfig and ValidateConfigBundle pass defaultLayerCaps; PutAgentConfigOverlay passes overlayLayerCaps.
func rejectCredentialMCP(doc map[string]any, joined string) error
// After the member stream. manifests: extensions/<id>/package.json bodies keyed by <id>;
// members: every regular member path. Rejects a file at extensions/<id>; each omp (else pi)
// .extensions entry must be a member under extensions/<id>/ whose extensionIDFromPath is <id>
// (D3). An unparseable package.json is skipped, as the SDK's readExtensionManifest skips it.
func lintExtensionManifests(manifests map[string][]byte, members map[string]bool) error
func extensionIDFromPath(p string) string // ports the SDK's getExtensionNameFromPath
```

Tests: each merge-table row; under `extensions/foo/`, `src/index.ts` (id
`src`) and `../bar/index.ts` rejected, `foo.ts` accepted; a file at
`extensions/foo` rejected; each D13 carrier rejected, `auth.tokenUrl`
userinfo and `?token=` included; an env name and `!command` accepted; MCP
drop only with an overlay; max-size layers fit the effective cap
(alias-heavy `models.yml`); `profiles/` and oversize layers rejected; an
orphan profile key warns; a row written around the door fails closed; no
overlay → default bytes; a put between the two statements ships a matching
pair; tenant isolation.

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

Runner per D8; `validateNestedRegularMember` and the YAML-mapping check
accept all three settings paths. Tests: an empty, unknown, or other-Runner
container rejected; a match returns only the version; other fetch errors fail
Start; an unchanged nudge skips `MountLabel` and Reload; a missing matched dir
re-fetches; a non-mapping settings layer fails.

### T4 — Agent: settings layers and removal filter

`readMountedSettingsPaths(currentDir): Promise<string[]>` replaces
`readMountedSettingsPath`; `buildFleetSettings` passes the list as
`configFiles`. Before `createAgentSession`, filter skills and extension entries
per D4. Tests (`cli.config-passthrough.test.ts`): a user layer overrides a
tenant setting spelled with a legacy key; `includeSkills: ["web-*"]` keeps
`web-search` and drops `notes`; `ignoredSkills: ["web-*"]` drops
`web-search`; `disabledExtensions: ["skill:foo"]` drops skill `foo`;
`disabledExtensions: ["extension-module:bar"]` drops `bar` and keeps `baz`.

### T5 — Server: overlay RPCs and effective read

Handlers in `go/server/agent_config_service.go`, all four `authenticatedOpen`;
every put and delete, default included, calls `SignalConfigChanged`. The boot
lint (D14):

```go
// go/internal/store/agent_config.go: validateAndHashConfigBundle(bundle, defaultLayerCaps) over
// CurrentAgentConfig; ErrNotFound → nil. Serve (go/server/serve.go) calls it right after
// seedBootstrapAccounts and passes a hit to failStartup.
func (s *Store) LintStoredAgentConfig(ctx context.Context) error
```

Tests: each authorization-table row, UNSPECIFIED and DEFAULT rejected; an agent
caller denied; the effective read names winning layers and refuses another
user's agent; `Serve` fails on a stored default with a literal MCP `env` value
or 3001 files, and starts on an absent or clean one.

### T6 — CLI and docs

`compass agent-config overlay push|delete|info --layer tenant|user` (required)
and `compass agent-config effective <agent-handle>`; default verbs unchanged.
`maxBundleFileCount` and `maxBundleContentBytes` become the D6 layer caps:

```go
// go/cmd/compass/bundle.go
type bundleCaps struct { files int; contentBytes int64 }
var (
    defaultBundleCaps = bundleCaps{files: 3000, contentBytes: 48 << 20}
    overlayBundleCaps = bundleCaps{files: 512, contentBytes: 4 << 20}
)
func buildBundle(dir string, caps bundleCaps) ([]byte, error)
func checkBundleCaps(caps bundleCaps, fileCount int, contentBytes int64) error
```

Tests: a 3001-file default, a 513-file overlay, and a 4 MiB + 1-byte overlay
fail, naming the cap. Docs: layer order, merge table, removal rules, MCP lint.

## Tasks

- [ ] T1 Store
- [ ] T2 Signal cutover
- [ ] T3 Keyed fetch and Start materialize
- [ ] T4 Agent settings layers and removal filter
- [ ] T5 Server RPCs
- [ ] T6 CLI and docs

## Decisions

- **D1** (DL-495) — Config layers default → tenant → user, resolved on the
  Server; the user is `agent_accounts.owner_user_id`.
- **D2** (DL-496) — Overlays are whole bundles in tenant-owned, FORCE-RLS
  `agent_config_overlays`; NULL `owner_user_id` is the tenant layer.
- **D3** (DL-497) — Members merge per type as the Merge rules table defines;
  MCP files stay per layer; an extension is a whole `extensions/<id>/`
  directory, and the door requires each manifest entry's SDK-derived id to
  equal `<id>`.
- **D4** (DL-498) — Removal: MCP `enabled: false` over the merged result when
  an overlay applies; rules by `ttsr.disabledRules`; skills and extensions by
  an agent-side filter that reproduces the SDK's matching rules.
- **D5** (DL-499) — Settings layer in the SDK: one file per layer as
  `configFiles`.
- **D6** (DL-500) — Every layer passes the `PutAgentConfig` door; overlays
  carry no `profiles/`; layer caps (default 48 MiB / 3000 files, overlay
  4 MiB / 512) keep every effective bundle within one 64 MiB / 4096-file cap.
- **D7** (DL-501) — `FetchAgentConfig` requires `container_name`, authorizes
  like `FetchSecrets`, and reads all layers in one statement.
- **D8** (DL-502) — Config materializes at Start on podman and host, and
  Start records the container's version; vsock skipped.
- **D9** (DL-503) — Effective version: the default version with no overlay,
  else hex sha256 over (`configMergeEpoch`, default, tenant, user).
- **D10** (DL-504) — `SignalConfigChanged` is a payload-free fleet-wide nudge;
  refresh fetches with `if_version` before `MountLabel`.
- **D11** (DL-505) — Overlays get separate `authenticatedOpen` RPCs with a
  required TENANT|USER layer; the default RPCs stay `adminOnly`.
- **D12** (DL-506) — `GetEffectiveAgentConfigInfo` reports each member's
  winning layer.
- **D13** (DL-511) — The door rejects, on every layer, MCP
  `oauth.clientSecret`, `auth.clientSecret`, `envPolicy`, `headerPolicy`, URL
  userinfo and secret-named query keys in `url`, `auth.tokenUrl`, and
  URL-shaped `args`, `exaApiKey=` in `args`, and any `env` or `headers` value
  that is neither a `!command` nor an env name; CD-3's MUST narrows to these
  fields.
- **D14** (DL-512) — Each boot lints the stored default with the default door
  before mounting doors and fails startup on a hit; the fetch re-checks only
  the Runner's rules, failing closed with a named error; an orphaned profile
  key on the effective bundle logs a Warn and delivers.

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
- **OQ-5 (load-bearing) — closing the MCP credential MUST.** No door rule over
  free-form strings proves a value is not a credential, so A and C narrow
  CD-3's "an MCP config MUST NOT embed a credential" to their listed fields;
  choosing A revises that MUST.
  - A. Field lint (D13). Residual: an identifier-shaped token, a `!command`
    that prints one, a token in `command` or another `args` element; the
    writer owns it, as today.
  - B. A, plus each overlay `env` or `headers` value must name an
    env-delivery secret visible at that layer's scope; no `!command`. Closes
    those two fields only, couples the door to secret declarations, and bars
    plain values such as `LOG_LEVEL: debug`.
  - C. A, plus a known-prefix scan (`ghp_`, `github_pat_`, `sk_live_`,
    `xoxb-`, `AKIA`) of every MCP string, `args` included. Unknown formats
    pass; the list needs upkeep.
  - **Recommendation: A, revising CD-3's MUST to D13's fields.** No option
    closes `command` or free-form `args`. A closes every credential-only field
    and every literal `env` or `headers` value; C can follow with no storage
    or fetch change.
- **OQ-6 (non-load-bearing) — tenant profiles.** D6 bars `profiles/` in
  overlays; lifting it means the registry guard reads every tenant's overlays.
- **OQ-7 (non-load-bearing) — scoped nudge.** An unchanged container costs one
  version-only round-trip (D10); targeting can wait for fetch metrics.
