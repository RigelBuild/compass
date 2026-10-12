# Compass agent config overlays — tenant and user layers

Refs: RIG-3112

> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../../agent/compass-agent-config-delivery/design.md` header).

**Named decisions this record introduces:** D1–D14 in
[Decisions](#decisions). The driver claims ledger ids with `bun tools/dl-claim`
and lands the rows under `docs/designs/decisions/agent/` in this same PR.

## Problem / Intent

Agent config is one fleet-wide bundle: `agent_config_bundle` is a singleton
row, fetched with no agent key, so every agent in every tenant gets the same
config. This record keeps that bundle as the global **default** layer, adds
a **tenant** and a **user** overlay on top, and has the Server resolve one
effective bundle per agent over the existing fetch and mount spine.

## Approach

### Ground truth

- **Default layer.** `agent_config_bundle` (`0001_init.sql`) is a
  credential-free singleton with no `tenant_id`, in the RLS-exempt `bucketA`
  list of `go/internal/store/rls_pgtest_test.go`.
- **Writers and roles.** `PutAgentConfig`/`DeleteAgentConfig` are `adminOnly`
  (`go/internal/auth/admin_gate.go`; `AdminGate.check` admits one account).
  Only `BootstrapAdmin` writes `UserRoleAdmin` (`go/internal/store/accounts.go`).
- **User coordinate.** `agent_accounts.owner_user_id` is set at insert and
  never updated; `SecretRecordsForAgent` already layers on it
  (`OR (s.scope_kind = 1 AND s.scope_id = a.owner_user_id)`). No other user
  model exists, so the user layer is the agent's owning user.

### Design

**Layers and order (D1, assumes OQ-4 A).** Effective bundle =
default → tenant → user; a missing layer is skipped. CD-1's post-MVP seam
("config keys on a persona / agent-level role, never an individual agent
id") stays open: a user is not an agent id, and a role layer would later add
a key column and a version-tuple slot (D9). A profile is not a layer (D6).

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

It ships in a new migration (Global Constraints). A user write always sets
`owner_user_id = caller`, so the owner is in the writer's tenant.

**Merge rules (D3, assumes OQ-1 B).**

| Member | Rule |
|---|---|
| `settings/config.yml` | Not merged on the Server. Each layer's file is emitted as `settings/config.yml`, `settings/tenant.yml`, `settings/user.yml` (D5). |
| `models.yml` | YAML deep-merge on the Server: objects recurse, arrays and scalars replace. The SDK reads one `models.yml` path, so it cannot layer it. No key normalization. |
| `mcp/*.json` | Kept per layer as `mcp/<layer>-<name>.json` (`default-` < `tenant-` < `user-` in sort order), so `readMountedMcpConfigs` merges by server name, later wins, and keeps per-file provenance. Drops follow D4. |
| `AGENTS.md` | Concatenate default, tenant, user with a blank line between. |
| `skills/<name>/` | Whole directory from the highest layer that has it. |
| `extensions/` | Keyed by id: `<id>` for `extensions/<id>.ts`, `extensions/<id>.js`, or `extensions/<id>/`. The highest layer with the id wins its whole entry; lower entries for that id, in either form, are dropped. |
| `rules/*`, `agents/*`, `prompts/*` | Whole file by path from the highest layer that has it. |
| `profiles/` | Default layer only (D6). |

With no overlay row, the effective bundle is the default bytes, unchanged.

**Removal (D4, assumes OQ-2 B).** Overlays add or replace members. Removal:

- **MCP.** When an overlay applies, the resolver removes every server whose
  winning merged entry has `"enabled": false` from every layer file. It must
  run here: the agent connects through `MCPManager.connectServers`, which
  ignores `enabled`. With no overlay the default bytes pass through and a
  default `enabled: false` stays inert, as today (byte-identical default).
- **Rules.** `ttsr.disabledRules` in any settings layer (the SDK applies it).
- **Skills and extensions.** `cli.ts` passes `skills: mounted.skills` and
  `disableExtensionDiscovery: true`, so `createAgentSession` (`sdk.ts`) skips
  both knobs. compass-agent filters first, with the SDK's rules: skill names
  match `skills.includeSkills`/`skills.ignoredSkills` by `new Bun.Glob(pattern)`,
  and `disabledExtensions` entry `skill:<name>` drops a skill
  (`extensibility/skills.ts`); `extension-module:<id>` drops an extension
  path whose `getExtensionNameFromPath` is `<id>` (`discoverExtensionPaths`).

**Settings layered by the SDK (D5).** `#loadOverlayYaml` (`settings.ts`) runs
`#migrateRawSettings` per file before `#deepMerge`, so a Server-side YAML
merge could invert layer order across legacy and current keys. The tree
carries one settings file per layer, and `buildFleetSettings`
(`packages/compass-agent/src/cli.ts`) passes those present as `configFiles`,
in layer order. The put door still accepts only `settings/config.yml`; the
Runner validator accepts all three paths (T3).

**One door, one effective cap (D6).** Every layer write runs the
`validateAndHashConfigBundle` door, MCP lint (D13) included. Overlays may not
carry `profiles/`, so the registry's orphan guard in `PutAgentConfig` stays
default-only; `COMPASS_PROFILE` selects from the effective bundle.

The effective bundle keeps today's door limit, 64 MiB / 4096 files
(`maxDecompressedBytes`, `maxFileCount`), within the Runner's unchanged
`maxConfigBytes`. Layer caps make every combination fit, so no write reads
another layer: the default door drops to 48 MiB / 3000 files, and each
overlay caps at 4 MiB / 512 files. The merge adds no files, and adds bytes
only through `AGENTS.md` separators and the re-encoded `models.yml`, which
the door counts at re-encoded size (aliases expand).

**Cross-layer checks (D14).** Per-value lints hold for a merge of clean
layers, because each value comes from one layer. The one cross-layer lint,
`lintProfileAgentKeys`, runs on the default at its door, so an overlay
`agents/x.md` with a new frontmatter `name:` can orphan a profile key. The
resolver logs a Warn naming the key and layer versions and delivers; the
lint calls such a key "a silent no-op at spawn". The fetch re-checks only the
cap and member paths, so a lint added later cannot fail a stored row. A
breach fails closed: `CodeInternal`, "effective config for agent %s invalid
(default %s, tenant %s, user %s): %v". Only a row written around the door,
or a pre-T1 default above 48 MiB under overlays, reaches it.

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

A version-only variant answers `if_version`; on a miss the full statement
runs and the version is recomputed from its rows (one snapshot).

The keyed fetch needs a container binding, which `Hub.enroll` clears. No
rebind path is needed: Runner `Run` (`go/internal/runner/run.go`) dials once,
sweeps this Runner's stale containers, and returns when `RunSessions` ends,
so no container outlives an enrollment. `lifecycleService.reprovision`
(`go/server/lifecycle.go`) re-creates a lost container, binding it before Start.

**Materialize at Start (D8).** `Hub.Provision` binds the container only after
the relay returns, so `Host.Provision` (`go/internal/runner/host.go`) cannot
fetch keyed; it now only runs `ensureRoot`. Start fetches (tracked version as
`if_version`) and materializes before exec, next to
`FetchSecretsByContainer`, then records `h.configVersions[name] =
mount.Version` for the refresh fast path. Podman uses the live `MountLabel`;
the host tier is unchanged; vsock is skipped, as `refreshOneContainer` skips
it. The `CodeFailedPrecondition` "no config surface" degrade stays; any other
fetch error fails the Start.

**Effective version (D9).** With no overlay row it is the default version;
otherwise lowercase hex sha256 (a path segment for `ConfigMaterializer`) over
the length-framed tuple (`configMergeEpoch`, default, tenant, user; `""` when
absent). A matched version with a missing `root/<version>` forces a full
re-fetch. An epoch bump sends no signal; it reaches a container at its next
Start or the next config write's nudge.

**Refresh signal (D10).** `SignalConfigVersion(version)` becomes
`SignalConfigChanged()`, a payload-free fleet-wide nudge sent by every
default or overlay put and delete. `refreshOneContainer` fetches with the
tracked version before `MountLabel`, and on a version-only response skips
the inspect, relabel, and Reload.

**Write and read surface (D11, D12, assumes OQ-3 A).** The default RPCs stay
unchanged and `adminOnly`. New `authenticatedOpen` RPCs carry the overlays:

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

Overlay RPCs accept only TENANT or USER (else `InvalidArgument`); USER always
addresses the caller's own layer. Agents have no write door.

| RPC | TENANT | USER |
|---|---|---|
| Put / Delete | `UserRoleAdmin`, checked after `requireUser` as the user-secret store does for `SecretScope` | caller |
| Overlay info (names only, as `GetAgentConfigInfo`) | any user in the tenant, since all their agents consume it | caller |
| Effective info | the caller's own agent, via `AgentByHandle(ctx, caller, handle)` | same |

Only the bootstrap tenant has an admin today, so the tenant layer is writable
only there until a path grants `UserRoleAdmin` elsewhere (out of scope).

**MCP credential lint (D13).** The delivery record says "an MCP config MUST
NOT embed a credential", yet `validateMCPMember` checks only JSON. The door
gains `rejectCredentialMCP` on every layer, the default included: each string
under `mcpServers.<name>.headers` and `.env` must pass `isEnvIndirection`
(env name or `!command`), as `rejectCredentialModels` requires for
`models.yml` headers. Cost: a literal such as `PORT: "8080"` needs the
`!command` form. Existing limit kept: an identifier-shaped literal passes.

**Trust boundary.** A user layer reaches only that user's agents; a tenant
layer reaches only its tenant and needs a tenant admin. Code-bearing members
(extensions, MCP `!command` values) run only for agents the writer controls.

## Alternatives considered

- **Member-granular overlay rows.** Rejected: a new per-member API and
  validator, and no atomic whole-layer push.
- **Layered mount, merge in the agent.** Rejected: Server-side resolution
  keeps one `current/` tree and one validated bundle to report (D12). D5
  keeps this hybrid for settings alone.
- **Revalidate dependent layers on every write.** Rejected for D14: a default
  write would read every tenant's overlays, and the only cross-layer lint
  guards a no-op.
- **Pending binding at Provision.** Rejected: a second binding lifecycle;
  Start already holds one.

## Plan

### Global Constraints

- Public repo: no private-product detail. The capability is "multi-tenant in
  one deployment" (`docs/designs/meta/oss-core-managed-boundary/design.md`).
- Schema: new `go/internal/store/migrations/0002_agent_config_overlays.sql`.
  Never edit `0001_init.sql`: `verifyChecksum` (`go/internal/store/store.go`)
  refuses an applied migration that changed. 0002 creates the table and
  indexes, grants table and sequence privileges to `compass_app,
  compass_system` (0001's grant covered only its own tables), enables and
  forces RLS with 0001's `tenant_isolation` policy, and adds the
  `set_updated_at` trigger. Add the table to `tenantOwned` in
  `rls_pgtest_test.go`. Never set `updated_at` by hand.
- RLS: overlay, owner, and default reads run as `compass_app` under the tenant
  GUC. The only system-role step on the fetch path is the existing
  `accountTenantContext` `AccountTenant` lookup `FetchSecrets` uses.
- No overlay passes a weaker door than `PutAgentConfig`.
- Clean cutover: no unkeyed `FetchAgentConfig`, `SignalConfigVersion`, or
  Provision-time fetch remain; migrate the fakes and test clients
  (`go/internal/runnerhub/config_fetch_test.go`,
  `go/server/runner_config_compose_pgtest_test.go`).
- Every PR builds and passes alone; red → green within each task.

### T1 — Store: migration, lints, merge, resolve

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
// go/internal/store/agent_config.go — every layer's door.
func rejectCredentialMCP(doc map[string]any, joined string) error
```

Tests: each merge-table row, including one extension id as a file in one
layer and a directory in another; MCP drop only with an overlay; max-size
layers (alias-heavy `models.yml` included) fit the effective cap;
`profiles/`, oversize layers, and literal MCP `headers`/`env` rejected; an
orphaned profile key delivers with a Warn; an oversize row written around the
door fails closed; no overlay → default version and bytes; a put between the
two statements still ships a matching pair; tenant isolation.

### T2 — Signal cutover

`ConfigVersion.version` is removed (number reserved) with every call site:
`Hub.SignalConfigChanged() error` replaces `SignalConfigVersion`
(`go/internal/runnerhub/config_signal.go`); the `signaler` interface and
`signalConfigVersion` in `go/server/agent_config_service.go`; the
`GetVersion()` log field in `go/internal/runner/dispatch.go`. Tests: a nudge
reaches each attached router; no-op with no live sessions.

### T3 — Keyed fetch and Start materialize

One PR, because the handler rejects an unkeyed request. Proto
`FetchAgentConfigRequest.container_name = 2`; handler per D7.

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

Runner: Provision runs `ensureRoot` only; Start per D8.
`validateNestedRegularMember` and the YAML-mapping check accept all three
settings paths. Tests: an empty, unknown, or other-Runner container gets
InvalidArgument or PermissionDenied; a match returns only the version frame;
other fetch errors fail Start; an unchanged nudge after Start skips
`MountLabel` and Reload; a missing matched dir forces a full re-fetch; both
new settings paths unpack, and a non-mapping one is rejected.

### T4 — Agent: settings layers and removal filter

`readMountedSettingsPaths(currentDir): Promise<string[]>` replaces
`readMountedSettingsPath`; `buildFleetSettings` passes the list as
`configFiles`. Before `createAgentSession`, filter skills and extension entry
paths per D4. Tests in `cli.config-passthrough.test.ts`: a user layer
overrides a tenant setting spelled with a legacy key; `ignoredSkills:
["web-*"]` removes `web-search`; `disabledExtensions: ["skill:foo"]` removes
skill `foo`; `disabledExtensions: ["extension-module:bar"]` removes `bar`
while `baz` loads.

### T5 — Server: overlay RPCs and effective read

Proto messages per D11/D12; handlers in `go/server/agent_config_service.go`;
the four procedures are `authenticatedOpen` in `go/internal/auth/admin_gate.go`.
Every put and delete, the default included, calls `SignalConfigChanged`.
Tests: the authorization table row by row (UNSPECIFIED and DEFAULT rejected; a
member reads tenant info but cannot write it); an agent caller is denied; the
effective read reports the winning layer for a default-only, a
tenant-replaced, and a user-replaced member, and refuses another user's agent.

### T6 — CLI and docs

`compass agent-config overlay push|delete|info --layer tenant|user` (required)
and `compass agent-config effective <agent-handle>`; default verbs unchanged.
Document layer order, the merge table, removal rules, and the MCP lint in the
agent-config docs.

## Tasks

- [ ] T1 Store: migration 0002, MCP lint, layer caps, merge, one-snapshot resolve
- [ ] T2 Signal cutover: payload-free `ConfigVersion` with all call sites
- [ ] T3 Keyed fetch, Start materialize, Runner settings paths
- [ ] T4 Agent: per-layer `configFiles`, skills/extensions filter
- [ ] T5 Server: overlay RPCs, authorization, effective read, nudges
- [ ] T6 CLI verbs, docs

Order: one linear stack, T1 → T2 → T3 → T4 → T5 → T6. T3 needs T1
(`EffectiveAgentConfig`) and T2. T5 needs T2 (`SignalConfigChanged`) and
lands after T3 and T4, so no overlay can be written before the Runner and
the agent consume it.

## Decisions

- **D1** (DL-495) — Config layers default → tenant → user, resolved on the
  Server; the user is `agent_accounts.owner_user_id`.
- **D2** (DL-496) — Overlays are whole bundles in tenant-owned, FORCE-RLS
  `agent_config_overlays`; NULL `owner_user_id` is the tenant layer.
- **D3** (DL-497) — Members merge per type as the Merge rules table defines;
  MCP files stay per layer; extensions merge by id across file and directory
  forms.
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
- **D13** (DL-511) — The door rejects literal MCP `headers` and `env` values on every
  layer; each must pass `isEnvIndirection`.
- **D14** (DL-512) — The fetch re-checks only the Runner's rules and fails closed with
  a named error; an orphaned profile key on the effective bundle logs a Warn
  and delivers.

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
