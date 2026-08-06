# Compass agent config passthrough — fleet OMP config delivery (settings, AGENTS.md, rules, subagent defs, models.yml)

Status: Draft

> **Amendment (2026-07-31) — settings/rules/AGENTS.md delivered by object
> injection.** The frozen record grounded CP-1/CP-2/CP-4's agent-side
> consumption on the vendored 17.1.8 fork (`forks/oh-my-pi`), whose `Settings`
> reads a `PI_CONFIG_FILES` env overlay. The runtime dependency is the
> installed `@oh-my-pi/pi-coding-agent@16.5.2`, which reads overlays ONLY via
> the `configFiles` option (`src/config/settings.ts:269`: "`this.#configFiles
> = options.configFiles?.map(file => path.resolve(this.#cwd,
> expandTilde(file))) ?? [];`"; a grep of the installed source for
> `PI_CONFIG_FILES` returns zero matches) — the record's env-var mechanism was
> inert. This amendment corrects the MECHANISM to in-process object injection
> at `createAgentSession`: settings as a `Settings.loadIsolated`
> `settingsManager`, rules as a `rules: Rule[]` object, and the fleet
> AGENTS.md composed into `contextFiles`; `agents/` and `models.yml` keep the
> agent-dir symlink seam (no object seam exists). Decisions, deliver/defer
> rulings, precedence semantics, and the credential posture (GC-5) are
> unchanged; the bundle + mount + `current/` flip (CD-3) remains the delivery
> vehicle. This ships against the current OMP pin (`^16.4.8`, resolving
> 16.5.2); the 16→17 upgrade is a separate change — the injection seams
> (`init`/`loadIsolated`, `rules`, `contextFiles`) are **assumed** unchanged
> in 17.1.8 and to be re-verified at upgrade time, not a load-bearing claim
> for the shipped mechanism.
>
> Freezes on merge; later changes supersede by citation, never rewrite
> (convention: `../compass-0.5/design.md:10-12`). Tracked as SEA-1678. Extends
> the frozen SEA-1568 record
> [`../compass-agent-config-delivery/design.md`](../compass-agent-config-delivery/design.md)
> (CD-1..CD-4) — this record adds bundle **categories** to that machinery and
> builds no new channel.
>
> **New named Decisions this record introduces (reserves ledger rows
> DL-123..DL-126; compass, as DECISIONS.md single-writer, appends them at
> ship = merge):**
>
> - **CP-1** — The fleet's OMP settings document is delivered by **whole-file
>   passthrough**: a `settings/config.yml` member in the existing fleet config
>   bundle, mounted read-only through `current/`, and handed to
>   `createAgentSession` as an in-process **`settingsManager`** built from the
>   mounted member via `Settings.loadIsolated({ cwd, agentDir, configFiles:
>   [settingsPath] })` (amended 2026-07-31; originally an inert
>   `PI_CONFIG_FILES` env-var scheme) — fleet settings override the checkout's
>   project settings, and only runtime overrides outrank them. No curated key
>   subset; TUI-only keys ride along harmlessly.
> - **CP-2** — Fleet AGENTS.md rides the same bundle as a top-level
>   `AGENTS.md` member: a **GLOBAL/user-level** working-conventions file the
>   entrypoint injects as `contextFiles: [fleetGlobal,
>   ...discoverContextFiles(cwd, agentDir)]` — the agent re-runs the SDK's own
>   project discovery itself, so the fleet file **composes additively** with
>   the checkout's own AGENTS.md chain (amended 2026-07-31; originally a
>   user-level symlink) — never workspace-placement into the checkout.
> - **CP-3** — The complete OMP config-dir surface is enumerated (the table
>   in §Approach) with an explicit deliver/defer ruling per category; the MVP
>   delivers **every category the current agent wave populates** — settings,
>   AGENTS.md, `rules/`, subagent definitions (`agents/`), and `models.yml`
>   (OQ-6, Matt 2026-08-03) — and **defers by name** commands, prompts,
>   instructions, hooks, custom tools, watchdog files
>   (`WATCHDOG.md`/`WATCHDOG.yml`), the `ConfigFile` family,
>   `settings.json`(legacy), `plugin-overrides.json`, LSP/DAP configs, and
>   `SYSTEM.md` (an **active in-container surface** — the checkout's
>   project-level `SYSTEM.md` loads today); only the TUI-entry
>   `APPEND_SYSTEM.md`/`TITLE_SYSTEM.md` overlays are structurally out of
>   scope — so no category can be "discovered late" again.
> - **CP-4** — The three agent-dir-anchored categories the wave populates —
>   `rules/`, `agents/` (subagent definitions), and `models.yml` — are all
>   delivered in the bundle; consumption is split by seam (amended
>   2026-07-31): `rules/` is injected as a **`rules: Rule[]`** object that
>   **composes** with the checkout's own discovered rules — the entrypoint
>   builds the fleet `Rule[]` from the mounted `rules/*.md` + `*.mdc` with the
>   SDK's own `buildRuleFromMarkdown`, re-runs the SDK's rule discovery itself,
>   and passes `[...fleetRules, ...discovered]` (fleet-first, both levels
>   load); while `agents/` and `models.yml` — which expose
>   **no object seam** — keep the user-level symlink pattern: the entrypoint
>   symlinks `$HOME/.omp/agent/agents` (dir) and `$HOME/.omp/agent/models.yml`
>   (file) at the mount's `current/`-relative paths, so the SDK's own
>   `getAgentDir()`-anchored discovery finds them natively and a
>   ConfigVersion flip stays live.

## Problem / Intent

SEA-1568/SEA-1674 landed the config-delivery seam, but the bundle carries only
three categories — the in-container reader "maps it into the three
`createAgentSession` option surfaces the SDK exposes — skills, extensions, and
MCP servers" (`packages/compass-agent/src/config-reader.ts:7-8`) — so a
dogfood container runs on **SDK-default settings** (compaction, context mode,
model roles, tool enablement all unconfigurable) and with **no fleet
AGENTS.md** (the SDK discovers AGENTS.md "by walking up from cwd"
(`forks/oh-my-pi/packages/coding-agent/src/discovery/agents-md.ts:4`), and
nothing in the container's cwd chain carries one unless the checkout happens
to). Matt's rulings (frozen inputs): whole-config-file passthrough, not a
curated subset; fleet-global for MVP; reuse the existing mount + ConfigVersion +
`current`-flip + Reload machinery — no second channel. This record designs
exactly that delta, plus the exhaustive sweep of every other config-dir
category the SDK reads, each with an explicit deliver/defer ruling. Matt's
OQ-6 ruling (2026-08-03) then expanded the MVP deliver set to every category
the current agent wave populates — adding rules/, subagent definitions
(agents/), and models.yml, delivered by CP-4 as amended — rules injected as a
`rules[]` object; agents/ and models.yml via the agent-dir symlink seam.

## Global Constraints

Inherited from the sibling record (`../compass-agent-config-delivery/design.md`)
and restated where this record's tasks touch them:

1. **The Server↔Runner inversion is frozen** (sibling GC-1). This record
   needs **no wire change at all**: carriage is the landed server-streaming
   `FetchAgentConfig` + `ConfigVersion` signal (DL-079), which moves opaque
   bundle bytes — new categories ride inside the tarball.
2. **Proto deltas are held-for-review** (sibling GC-2). The only `.proto`
   delta here is on the public `compass.proto`
   (`GetAgentConfigInfoResponse`, T1) — named in this record, landed in its
   implementation PR after `compass.v1`-owner review, buf-breaking-checked
   (additive fields only).
3. **The container is immutable after create; the agent is exec-driven**
   (sibling GC-3). All new categories are *contents* of the existing
   parent-dir mount: the Runner appends the config mount at `Launch`
   (`go/internal/runner/host.go:168`:
   `spec.Mounts = append(spec.Mounts, runtime.Mount{HostPath: mount.HostPath,
   ContainerPath: agentConfigMountPath, ReadOnly: true})`) and a later
   `current` flip is live inside the running container. No mount-set change.
4. **One env channel** (sibling GC-4). The settings file is not an env
   surface; env-vars stay exclusively on SEA-1327's secret surface. Nothing
   here writes `-e`/`Env`.
5. **Nothing secret-bearing enters the bundle** (sibling GC-7 + CD-3's
   credential-free rule, extended): the OMP settings schema marks credential
   paths — "Whether a setting holds a credential and must never be printed or
   exported" (`forks/oh-my-pi/packages/coding-agent/src/config/settings-schema.ts:5493-5495`,
   `isCredential`) — so the MVP rule is: **the fleet settings file MUST NOT
   set credential-marked keys**; provider credentials ride the auth seed
   (`packages/compass-agent/src/cli.ts:67-69`, `authSeedPath`) and tool
   secrets ride the aggregate env file. Enforcement is DECIDED (OQ-2, Matt
   2026-08-03): a **generated credential-key denylist at the server door**
   (authoritative — it survives a raw `PutAgentConfig`,
   `go/gen/compass/v1/compassv1connect/compass.connect.go:167`) plus a
   client-side warning in `compass agent-config push` (T1/T5). The delivered
   `models.yml` (CP-4) is covered by the same door denylist — both its
   credential surfaces (`providers.<name>.apiKey` and any
   `providers.<name>.headers.*` set to a literal secret) are rejected (the
   CP-3 models.yml row).
6. **Fleet-config writes are operator-scoped** (sibling GC-8). Unchanged:
   the same `PutAgentConfig` door carries the new members; no new RPC, no new
   gate classification.
7. **Go stack conventions** (sibling GC-6): `%w`-wrapped, stage-tagged errors,
   `context.Context` first; all Runner/store deltas follow
   `go/internal/store/agent_config.go` / `go/internal/runner/config_materialize.go`
   house style.
8. **Fleet-global granularity** (Matt's ruling #2, = DL-078's scope): one
   settings file and one AGENTS.md for the whole fleet; persona/role-keyed
   bundles remain the named post-MVP seam.

## Approach

The landed machinery is deliberately **category-agnostic** below the
validation layer: the Server stores an opaque gzip tarball
(`go/internal/store/migrations/0008_agent_config.sql:37-38`: "bundle: the
gzip-tarball bytes as delivered — transport for the skills/, extensions/, and
mcp/ material"), the Runner streams and unpacks it
(`go/internal/runner/config_fetch.go:54`, `config_materialize.go:96-99`), and
the update path re-materializes + Reloads
(`go/internal/runner/host.go:588` `RefreshConfig`, dispatch coalescing at
`go/internal/runner/dispatch.go:90-98`). The only places that *know* the
category set are three whitelists/readers:

- the store door (`go/internal/store/agent_config.go:405-406`: "bundle member
  %q is not under skills/, extensions/, or mcp/"),
- the Runner unpack (`go/internal/runner/config_materialize.go:57-61`:
  `configTopDirs = map[string]struct{}{"skills": {}, "extensions": {},
  "mcp": {}}`; `:442-443` rejects anything else),
- the in-container reader (`packages/compass-agent/src/config-reader.ts:12-17`:
  `skills/<name>/…`, `extensions/<name>/…`, `mcp/<name>.json`, `version`).

So the whole design is: **extend the bundle grammar by the delivered member
set (settings, AGENTS.md, rules/, agents/, models.yml), extend the three
whitelists, and wire each member into the seam the SDK already exposes.**
No new RPC, no new mount, no new update path.

### Decision CP-1 — settings delivery: whole-file passthrough via the `configFiles`/`settingsManager` injection seam

**What "config.json" concretely is.** The OMP SDK's settings document is the
main config file in the agent dir — canonical filename `config.yml`
(`forks/oh-my-pi/packages/utils/src/dirs.ts:25-26`: "Ordered main settings
filenames: canonical write target first … `MAIN_CONFIG_FILENAMES =
["config.yml", "config.yaml"]`"); `settings.json` is the *legacy* format the
SDK migrates away from (`forks/oh-my-pi/packages/coding-agent/src/config/settings.ts:1300-1301`:
"1. Migrate from settings.json" reading
`path.join(this.#agentDir, "settings.json")`). Its schema is the single
source of truth for everything Matt named: compaction
(`settings-schema.ts:5551` `CompactionSettings`), model roles
(`settings.ts:36` `ModelRole`; role storage `settings-schema.ts:72`
`ModelRoleStorage`), tool enablement, context handling, retry/thinking
budgets, and the TUI-only appearance keys that ride along harmlessly in a
headless process. This record therefore passes through **the OMP settings
document** as the bundle member `settings/config.yml` — **YAML, and only
YAML** (OQ-1, Matt 2026-08-03: "yml, and only yml so it's simple"): the door
admits exactly that one filename and grammar, with no `.yaml`/`.json`
variants and no author-as-JSON latitude.

**How the SDK reads it natively.** `createAgentSession` resolves settings as
"`const settings = await (options.settings ?? options.settingsManager ??
logger.time("settings", Settings.init, { cwd, agentDir }));`"
(installed `@oh-my-pi/pi-coding-agent@16.5.2`, `src/sdk.ts:1154-1156`) and
wires the result into the capability layer via
`initializeWithSettings(settings)` (`src/capability/index.ts:251`,
`sdk.ts:1157`) — a caller-provided `settingsManager` fully replaces the SDK's
own `Settings.init`, and the option is typed `settingsManager?: Settings |
Promise<Settings>` (`dist/types/sdk.d.ts:192`). The `Settings` constructor
consumes an overlay list from the **`configFiles` option**:
"`this.#configFiles = options.configFiles?.map(file =>
path.resolve(this.#cwd, expandTilde(file))) ?? [];`"
(`src/config/settings.ts:269`), loads each strictly (`settings.ts:801-823`,
`#loadOverlayYaml` — a missing or malformed overlay is a **hard error**:
"Config overlay not found", "Config overlay must be a YAML mapping"), and
merges with overlay-over-project precedence: "`this.#merged =
this.#deepMerge(this.#deepMerge({}, this.#global), this.#project);
this.#merged = this.#deepMerge(this.#merged, this.#configOverlay);
this.#merged = this.#deepMerge(this.#merged, this.#overrides);`"
(`settings.ts:1376-1378`; overlays folded per file at `:790-791`). (The
frozen record grounded this seam on the 17.1.8 fork's `PI_CONFIG_FILES` env
read — the installed 16.5.2 runtime has **no such read**: a grep of its
source for `PI_CONFIG_FILES` returns zero matches. Hence this amendment.)

**The mechanism (amended 2026-07-31).** The bundle gains one member,
`settings/config.yml`, door-validated as a YAML mapping (T1/T2). Nothing new
is materialized — the file lands inside the existing per-container mount at
`/run/compass/agent-config/current/settings/config.yml`. The entrypoint
(`main()`, `packages/compass-agent/src/cli.ts:301`) checks the reader for
the member and, when present, builds an isolated fleet Settings instance —
`Settings.loadIsolated({ cwd, agentDir, configFiles: [settingsPath] })` —
and passes it to `createAgentSession` as `settingsManager` (T4).
**`loadIsolated`, NOT `Settings.init`:** `init` caches a global singleton —
"`static init(options: SettingsOptions = {}): Promise<Settings> { if
(globalInstancePromise) return globalInstancePromise;`"
(`settings.ts:289-290`) — so once anything has initialized it, a later
caller's `configFiles` are silently ignored; `loadIsolated` — "`static
loadIsolated(options: SettingsOptions = {}): Promise<Settings> { const
instance = new Settings(options); return instance.#load(); }`"
(`settings.ts:324-327`) — always constructs fresh with our overlay, without
touching the singleton (which the `sdk.ts` session path never reads). The
build is guarded by a container-side Bun `YAML.parse` check (T4) — the
overlay loader hard-errors on a missing or malformed path
(`settings.ts:801-823`) and an unconfigured fleet must boot clean (the
reader's tolerant posture, `config-reader.ts:19-23`: "UNCONFIGURED … is a
VALID empty state"). When the member is **absent**, the entrypoint simply
passes no `settingsManager`, and the SDK falls through to its own default
`Settings.init({ cwd, agentDir })` (`sdk.ts:1154-1156`) — "unconfigured →
SDK defaults" holds by construction; there is no env channel to clear.

**Why overlay, not the global `config.yml`.** Materializing into
`$HOME/.omp/agent/config.yml` (the write target, installed `@16.5.2`
`settings.ts:268`: `this.#configPath = options.inMemory ? null :
path.join(this.#agentDir, MAIN_CONFIG_FILENAMES[0]);`) would make the fleet
file the SDK's *persistence target* — the SDK background-saves settings
changes to it (`settings.ts:1322` `#queueSave`), which cannot work against a
read-only mount and would fork fleet truth if copied. The overlay layer is
read-only by construction, and
its precedence is the right one for fleet policy: it beats the checkout's
project settings (a repo cannot override the operator) and loses only to
runtime overrides. Consequence, stated: a checked-in `.omp/config.yml` in the
agent's checkout still loads as the project layer but **loses every key the
fleet file sets**.

**Precedence vs `COMPASS_MODEL`.** The Runner-set model selector
(`go/internal/runner/agent_exec.go:79-81`) flows to `createAgentSession` as
`modelPattern` (`cli.ts:397`) and stays authoritative for the **main** model;
the fleet settings' `modelRoles` govern the role-resolved models (compaction,
subagent, etc.). See Open Questions (OQ-3) for the assumption's edge.

**Reload coverage — free.** Settings are read once per process when the
entrypoint builds the fleet instance (`Settings.loadIsolated`); the update
path is already an in-place agent **re-exec**
(`go/internal/runner/host.go:551-556`: RefreshConfig "re-materializes the
current config bundle into EACH live session's own per-container root … and
Reloads the agent in place" when the version moved), so the fresh process
re-reads `current/settings/config.yml` and rebuilds. The object path also
enables a later **in-process** refresh — rebuild the objects and re-create
the session (rebuild+resume) without a re-exec — named, not built here. No
new update machinery (T6 only adds test coverage).

### Decision CP-2 — AGENTS.md delivery: bundle member injected as a composed `contextFiles` global, not workspace-placement

**Why it cannot be a plain mount read.** The standalone-AGENTS.md provider
walks **up from cwd**: "Walk up from cwd looking for AGENTS.md files … `let
current = ctx.cwd; while (true) { const candidate = path.join(current,
"AGENTS.md")`" (installed `@oh-my-pi/pi-coding-agent@16.5.2`,
`src/discovery/agents-md.ts:25-29`), stopping at the repo root ("`if (current
=== (ctx.repoRoot ?? ctx.home)) break`", `agents-md.ts:50`). The container
agent's cwd is the checkout: the
Runner execs the agent `InDir(e.Workdir)` and exports
`COMPASS_WORKDIR` (`go/internal/runner/agent_exec.go:76-78`), and the
entrypoint keys the session to it (`cli.ts:337`:
`const cwd = env.COMPASS_WORKDIR?.trim() || process.cwd()`). The mount at
`/run/compass/agent-config` is not on that walk path.

**The chosen seam (amended 2026-07-31).** The fleet AGENTS.md is a
**GLOBAL/user-level** working-conventions file. The SDK exposes a direct
object seam for context files: `createAgentSession` short-circuits its own
discovery whenever the option is provided — "`const contextFilesPromise =
options.contextFiles ? Promise.resolve(options.contextFiles) :
logger.time("discoverContextFiles", discoverContextFiles, cwd, agentDir);`"
(installed `@oh-my-pi/pi-coding-agent@16.5.2`, `src/sdk.ts:1177-1179`),
where the option is an array of `{ path, content }` ("Context files
(AGENTS.md content). Default: discovered walking up from cwd",
`dist/types/sdk.d.ts:133-137`). So: the bundle gains a top-level `AGENTS.md`
file member, and the entrypoint reads it as a `{ path, content }` object and
passes `contextFiles: [fleetGlobal, ...(await discoverContextFiles(cwd,
agentDir))]` to `createAgentSession` (T4). The content is re-read from the
mount's `current/` path at each boot and the update path is a re-exec, so a
ConfigVersion flip + Reload re-resolves the new content with zero extra
wiring — the same parent-mount liveness CD-3 built
(`config_materialize.go:3-6`). When the member is absent the key is
**omitted**, so the SDK runs its own discovery — the tolerant absent→empty
posture, object-shaped.

**Composition, not replacement — the decision holds; the composing is
manual.** Providing `contextFiles` **short-circuits ALL discovery**
(`sdk.ts:1177-1179` above), and the SDK's own discovery is **project scope
only**: `discoverContextFiles` is the cwd walk-up — "Discover context files
(AGENTS.md) walking up from cwd. Returns files sorted by depth (farther from
cwd first, so closer files appear last/more prominent)"
(`src/sdk.ts:772-773`) — an alias over `loadProjectContextFiles`
("`loadProjectContextFiles as loadContextFilesInternal`", `src/sdk.ts:136`),
stopping at the repo root/home (`src/discovery/agents-md.ts:28-50`); there
is no user-level provider on this path for an injected file to layer under.
So the entrypoint **re-runs that same project discovery itself** — calling
the SDK's exported `discoverContextFiles(cwd, agentDir)`
(`src/sdk.ts:775-782`), no re-implemented walk-up — and prepends the fleet
global: fleet FIRST = least prominent, the user-level position, since the
sort puts closer-to-cwd files last/more prominent. The checkout's own
AGENTS.md chain keeps loading, automatic and unmodified — additive
composition, exactly the frozen decision, now composed by the entrypoint
rather than by native user/project layering (which object injection
collapses). One boot-once limit, named: `contextFiles` is resolved once at
session build (`sdk.ts:1471-1473`) and reused by later system-prompt
rebuilds (`sdk.ts:2476`), so this covers the boot cwd's repo — roaming
multi-repo pickup is SEA-1698 (filed, out of scope).

**Persona overlap, reconciled.** `COMPASS_PERSONA` is a **system-prompt
identity overlay** appended after the default prompt (`cli.ts:424-431`:
"Persona is an identity OVERLAY … append it after the default prompt");
AGENTS.md is a **context file** (project/user working context). Different
blocks, different semantics — no collision, and neither replaces the other.
The fleet AGENTS.md is the right home for fleet-wide working conventions;
persona remains per-agent identity.

One overlap folded from review: the container's system-prompt overlay channel
is **not** only `COMPASS_PERSONA`. Project/user `SYSTEM.md` also feeds the
prompt — `createAgentSession` → `buildSystemPromptInternal` (`sdk.ts:2720`) →
`loadSystemPromptFiles` (`system-prompt.ts:391-399`) runs whenever the caller
passes no `customSystemPrompt` (gate `callerControlsCustomPrompt`,
`system-prompt.ts:653-658`), and the compass entrypoint passes only
`systemPrompt` (a post-processing fn, `cli.ts:424-431`). That overlap is
repo/checkout-controlled and pre-existing — see the CP-3 table's SYSTEM.md
row. Persona and AGENTS.md themselves remain non-colliding as stated above.

### Decision CP-3 — the complete OMP config-dir surface: deliver/defer table

The SDK's generic resolver is `getConfigDirs(subpath)` over user
(`~/.omp/agent`, `~/.claude`, …) and project (`.omp/`, `.claude/`, …) bases
(`forks/oh-my-pi/packages/coding-agent/src/config.ts:126-151`), plus the
native discovery provider's own dir walk
(`discovery/builtin.ts:57-72`). Every category either resolver serves, with
its ruling. **In the container**: "user" = `$HOME/.omp/agent` (Runner-scoped
HOME, empty unless we populate it), "project" = `.omp/` inside the checkout
(repo-controlled, loads today).

| Category | Loader (file:line) | Delivered today? | This record | Rationale |
| --- | --- | --- | --- | --- |
| Settings (`config.yml`, the "config.json" surface; bundle member yml-only per OQ-1) | overlay via the `configFiles` option (installed `@16.5.2` `config/settings.ts:269`), resolved as `settings ?? settingsManager ?? Settings.init` (`sdk.ts:1154-1156`); merge order `settings.ts:1376-1378` | No — SDK defaults | **Deliver (CP-1)** | The headline gap: compaction/context/model-roles/tool enablement all live here. Injected as a `Settings.loadIsolated` `settingsManager` (amended 2026-07-31) |
| AGENTS.md (context files) | cwd walk-up `discovery/agents-md.ts:25-50` via `discoverContextFiles` (`sdk.ts:775-782`, project scope only); option short-circuit `sdk.ts:1177-1179` | No fleet channel (checkout's own loads) | **Deliver (CP-2)** | Fleet working conventions; injected as `contextFiles: [fleetGlobal, ...discoverContextFiles(cwd, agentDir)]` — COMPOSED, the agent re-runs project discovery itself (amended 2026-07-31) |
| Skills | `discovery/builtin.ts:276-289` (`.omp/skills`, `agentDir/skills`) | **Yes** — mount + `skills:` option (`config-reader.ts:75`, `cli.ts:417`) | Already delivered | SEA-1568/1674 |
| Extensions | `discovery/builtin.ts:473`, `:565` (`extensions/` per config dir) | **Yes** — `additionalExtensionPaths` + `disableExtensionDiscovery` (`cli.ts:418-419`) | Already delivered | SEA-1568/1674 |
| MCP servers (`mcp.json`/`.mcp.json`) | `discovery/builtin.ts:197-200` | **Yes** — mounted `mcp/*.json` + `enableMCP: false` (`cli.ts:420-421`) | Already delivered | SEA-1568/1674 |
| Slash commands (`commands/*.md`) | `discovery/builtin.ts:334-335`; `extensibility/custom-commands/loader.ts:106` | No | **Defer** | Interactive slash surface; the headless socket-driven agent takes no slash input |
| Rules (`rules/*.md`,`*.mdc`) | option short-circuit: "`options.rules !== undefined ? { items: options.rules, warnings: undefined } : await loadCapability<Rule>(ruleCapability.id, { cwd })`" (installed `@16.5.2` `sdk.ts:1434-1436`) | No | **Deliver (CP-4)** | The wave ships 18 rule files and the fleet AGENTS.md (CP-2) references them by `rule://` name — delivering AGENTS.md without rules/ ships dangling references. The fleet `Rule[]` (built from the mounted `rules/*.md` + `*.mdc` with `createSourceMeta` + `buildRuleFromMarkdown`) **composes** with the checkout's discovered rules: the entrypoint re-runs `loadCapability<Rule>(ruleCapability.id, { cwd })` itself and passes `[...fleetRules, ...discovered]`, fleet-first (amended 2026-07-31) |
| Prompts (`prompts/*.md`) | `discovery/builtin.ts:424-425` | No | **Defer** | TUI prompt-picker surface |
| Instructions (`instructions/*.md`) | `discovery/builtin.ts:631-632` | No | **Defer** | Same class as rules but absent in the wave (0 files); a future deliver ruling reuses the CP-4 seam |
| Hooks (`hooks/pre`,`hooks/post`) | `discovery/builtin.ts:665-677` | No | **Defer** | Executable code with a security surface; needs its own review (extensions already cover code injection under the operator trust model) |
| Custom tools (`tools/`) | `discovery/builtin.ts:725-740`; note cwd `.omp/tools` loads unconditionally (`cli.ts:413-415`: "sdk.ts:1861 runs discoverCustomToolPaths([], cwd) unconditionally") | No fleet channel (checkout's loads) | **Defer** | Executable; MCP + extensions are the delivered tool channels |
| Subagent definitions (`agents/`) | `task/discovery.ts:73-78` (`getConfigDirs("agents", { project: false })` filtered to the `.omp` source → `$HOME/.omp/agent/agents`: `config.ts:9-10`, `:83-86` + `dirs.ts:211-213`); defs are flat `.md` files (or symlinks) with frontmatter, `discovery.ts:43-46` + `parseAgent` | No | **Deliver (CP-4)** | The wave's `task` calls name exactly these defs (`agent: "design"`/`"implement"`/…), which the in-container `task` tool must resolve. Reconciled with the old rationale: "Compass owns agent topology server-side" governs **spawn/persona topology** (SpawnPeer/roles) — a different surface from the in-container subagent *definitions* the `task` tool reads; deferring them leaves the wave's `task` calls with no definitions to resolve. Dir symlink `$HOME/.omp/agent/agents` → `current/agents` |
| Watchdog files (`WATCHDOG.md`) | a **third resolver**, `collectConfigCandidates` (`advisor/watchdog.ts:53-135`): user level (agent dir) + a cwd→repoRoot walk probing `<F>` and `.omp/<F>`; WATCHDOG.md at `watchdog.ts:127-128`; consumed unconditionally in `createAgentSession` (`sdk.ts:1261-1263` `discoverWatchdogFiles`), injected into advisor prompts (`sdk.ts:3117-3133`) | No fleet channel (checkout's own loads) | **Defer** | Same class as rules — advisor attention prompts; headless-relevant |
| Advisor configs (`WATCHDOG.yml`/`.yaml`) | same `collectConfigCandidates` path (`advisor/config.ts:137-138` `discoverAdvisorConfigs`; consumed `sdk.ts:1263`) | No fleet channel | **Defer** | Same class as rules; rides the WATCHDOG.md decision |
| `models.yml` (provider overrides + custom model definitions) | `ModelsConfigFile` (`models-config.ts:105`) → default path `getAgentDir()/models.yml` (`config-file.ts:137-141`); the session path constructs `ModelRegistry` with no explicit `modelsPath` (`sdk.ts:1128-1130`) so the default holds; relocated `model-registry.ts:867`, loaded `:1466` (`tryLoad`) | No — SDK defaults | **Deliver (CP-4)** | The wave's routing/reasoning config (effort ladders, transport compat) lives here, and CP-1's settings file **cannot** express model *definitions* (`modelRoles` selects models; models.yml defines them, `model-registry.ts:1449-1452`). File symlink `$HOME/.omp/agent/models.yml` → `current/models.yml`. **Credential rule (GC-5, decided):** the schema carries TWO credential surfaces — provider `apiKey` (`models-config.ts:23`; installed into the resolver at `model-registry.ts:1548-1552`) and `headers` (`models-config.ts:22`; header values materialize into outbound auth value-first, `model-registry.ts:350-352`) — so the OQ-2 door denylist rejects `providers.<name>.apiKey` AND any `providers.<name>.headers.*` set to a non-env-indirection literal (CP-4's credential posture); the wave's own file is credential-free (routing only; baseUrl/apiKey ride env) |
| `ConfigFile` family (catch-all: `keybindings.yml`, smithery auth file, …) | every `ConfigFile` instance defaults into the agent dir as `getAgentDir()/<id>.yml` (`config-file.ts:139-141`) | No | **Defer** | `keybindings.yml` is TUI-only; the smithery auth file is credential-class (out of scope like model auth). Any future `ConfigFile` lands in the agent dir and is covered by the prevention property below |
| Legacy `settings.json` (per config dir; also its `extensions` key) | `discovery/builtin.ts:474`, `:853`; migration `settings.ts:1300-1304` | No | **Defer (never)** | Legacy format the SDK migrates away from; CP-1 delivers the canonical document |
| `plugin-overrides.json` | `extensibility/plugins/loader.ts:61` (`getConfigDirPaths("plugin-overrides.json", { user: false })` — project-only) | No | **Defer** | Project-level only by loader contract; repo-controlled already |
| LSP config | `lsp/config.ts:371-383` (filenames per config dir) | No | **Defer** | Devenv concern; DL-025 makes the image/agent own its devenv |
| DAP config | `dap/config.ts:134-144` | No | **Defer** | Same as LSP |
| `.env` (SDK dotenv autoload) | eager **import-time** load of `$HOME/.env`, configRoot/.env, agentDir/.env, and **cwd/.env** (`env.ts:196-213`), with `OMP_*`→`PI_*` mirroring (`env.ts:169-173`) | **Yes** — the checkout's own `.env` autoloads (cwd is the checkout, `agent_exec.go:76-78`, `cli.ts:337`) | **Defer / never** | Env rides SEA-1327 / GC-4, never the bundle. (Historical: the frozen record's T4 cleared `PI_CONFIG_FILES`/`OMP_CONFIG_FILES` against a repo-`.env` injection vector; the installed 16.5.2 runtime never reads that env var and settings now ride the `settingsManager` object, so that vector does not exist — amended 2026-07-31) |
| `SYSTEM.md` (system-prompt customization) | `createAgentSession` → `buildSystemPromptInternal` (`sdk.ts:2720`) → `loadSystemPromptFiles` (`system-prompt.ts:391-399`) whenever no `customSystemPrompt` is passed (gate `callerControlsCustomPrompt`, `system-prompt.ts:653-658`); the builtin provider reads BOTH `getAgentDir()/SYSTEM.md` and the nearest project config dir's `SYSTEM.md` (`builtin.ts:235-259`); result feeds the prompt as `systemPromptCustomization` (`system-prompt.ts:822-838`) | **Yes (project level)** — the checkout's `.omp/SYSTEM.md` loads today: the compass entrypoint passes only `systemPrompt` (a post-processing fn, `cli.ts:424-431`), so the gate is open | **Defer** | An **ACTIVE in-container surface**, not TUI-only: user level would activate the moment a fleet file landed at `$HOME/.omp/agent/SYSTEM.md`. The checkout-controlled project SYSTEM.md is pre-existing behavior outside this record's delta |
| `APPEND_SYSTEM.md` | `main.ts:817-825` (`findConfigFile`, consumed at `main.ts:864-866`) | N/A in-container | **Out of scope (structurally)** | Loaded by the **TUI/CLI entry** (`main.ts`) only — genuinely not on the `createAgentSession` path |
| `TITLE_SYSTEM.md` | `system-prompt.ts:302-310`, consumed `main.ts:384` and `interactive-mode.ts:1149` | N/A in-container | **Out of scope (structurally)** | Same TUI-only path |
| Model auth / provider credentials | `sdk.ts:1201` (`discoverAuthStorage(agentDir)`); seed override `cli.ts:440` (`session.agent.getApiKey = createSeedApiKeyResolver(home)`) | **Yes** — SEA-1327 auth seed | Out of scope | Credentials never ride the bundle (GC-5) |

The prevention property, restated: every config read above is anchored on
`getAgentDir()` or the project config dir — but through **three resolvers**,
not two: `getConfigDirs`/`findConfigFile` (`config.ts:126-151`), the discovery
providers (`discovery/builtin.ts:57-72`), and the advisor's
`collectConfigCandidates` (`advisor/watchdog.ts:53-135`) — plus the
`ConfigFile` default-path constructor (`config-file.ts:139-141`) and the
import-time dotenv autoload (`env.ts:196-213`). A reviewer of an SDK fork bump
therefore checks **any new read anchored on `getAgentDir()` / the project
config dir** against this table — not merely new `getConfigDirs`/`builtin.ts`
consumers (that two-resolver framing missed the watchdog surface).

One populated wave tree deliberately not in the table: `cotal/` is not an
OMP-SDK config surface (it belongs to the mesh/coordination layer, with its
own delivery channel) — out of scope here, noted so the OQ-6 enumeration is
auditable.

### Decision CP-4 — agent-dir-anchored categories: rules/ as an injected object; agents/ and models.yml via user-level symlinks

**The scope ruling (OQ-6, Matt 2026-08-03).** The MVP delivers every config
category the current agent wave actually populates. Beyond CP-1/CP-2 that
adds three: `rules/` (18 files), `agents/` (6 subagent definitions: design,
design-critic, implement, implement-hard, review, task), and `models.yml`
(routing/reasoning only, credential-free). They are a coherent set with
CP-2: the fleet AGENTS.md references rules by `rule://` name, and the wave's
`task` calls name exactly the `agents/` definitions — delivering AGENTS.md
alone would ship dangling references and unresolvable subagents. Categories
the wave does NOT populate stay deferred (the CP-3 table).

**Why a fourth decision, not an extension of CP-1 or CP-2 (amended
2026-07-31).** CP-1 injects settings as a `settingsManager` object and CP-2
injects the fleet AGENTS.md as `contextFiles`; this decision covers the
three remaining wave-populated categories, split by whether the SDK exposes
an object seam. **Rules do:** `createAgentSession` takes a `rules` option
that short-circuits discovery — "`options.rules !== undefined ? { items:
options.rules, warnings: undefined } : await
loadCapability<Rule>(ruleCapability.id, { cwd })`" (installed
`@oh-my-pi/pi-coding-agent@16.5.2`, `src/sdk.ts:1434-1436`). Because the
option short-circuits the SDK's own discovery, the entrypoint COMPOSES
explicitly: it builds the fleet `Rule[]` from the mounted `rules/*.md` +
`*.mdc` with the SDK's own helpers — `createSourceMeta(provider, filePath,
"user")` (`src/discovery/helpers.ts:124`) and `buildRuleFromMarkdown(name,
content, filePath, source, { stripNamePattern })` (`dist/types/discovery/
helpers.d.ts:94`), producing `Rule`s carrying the required `_source:
SourceMeta` (`dist/types/capability/rule.d.ts:29-52`;
`dist/types/capability/types.d.ts:71-80`) — then re-runs the SDK's own rule
discovery itself (`loadCapability<Rule>(ruleCapability.id, { cwd })`,
`capability/index.ts:228`) and passes `[...fleetRules, ...discovered]`. Same
construction the builtin provider uses, so frontmatter parses identically;
fleet-first ordering, both levels load. **Subagent defs
and models.yml do not:** subagent definitions are discovered by walking the
agent dir (`getConfigDirs("agents", { project: false })` →
`$HOME/.omp/agent/agents`, `task/discovery.ts:73-78`); the `agentRegistry?`
option is IRC routing, not definition injection. models.yml is loaded by
the `ModelRegistry` from `getAgentDir()/models.yml`
(`config-file.ts:137-141`, `sdk.ts:1128-1130`); only `modelRegistry?` (a
fully built registry) exists — no raw-yml object param. A documented
exception, and a possible future SDK follow-up. Those two keep CP-2's
original symlink pattern — the entrypoint (T4) creates

- `$HOME/.omp/agent/agents` → `/run/compass/agent-config/current/agents` (dir symlink),
- `$HOME/.omp/agent/models.yml` → `/run/compass/agent-config/current/models.yml` (file symlink),

each `current/`-relative so a ConfigVersion flip + Reload re-resolves new
content with zero extra wiring (the CD-3 flip-liveness), each created only
when the member exists and removed (symlink-only, never a user-placed
regular file or real directory) when it does not — the idempotent, tolerant
posture, verbatim.

**Why a dir symlink for agents/, not per-file links.** Subagent discovery
reads the *directory* — it `readdir`s the agents dir accepting files AND
symlinks ending `.md` (`task/discovery.ts:43-46`). A single dir symlink
delivers the whole set atomically with the `current` flip; per-file links
would need create/remove reconciliation per member for zero gain. This
follows the skills precedent — the mount already serves whole skill dirs to
the SDK (`config-reader.ts:12`).

**Composition.** agents/ and models.yml land at USER level, composing with
the checkout exactly as before: the checkout's own `.omp/agents` keeps
loading as project-level entries with the SDK's own precedence (project
over user for subagent defs, `task/discovery.ts:62-67`); models.yml has no
project layer — the user-level file is the only one. Rules, now injected,
**compose** the same way: providing `rules` short-circuits the SDK's own
discovery (`sdk.ts:1434-1436`), so the entrypoint runs that discovery ITSELF
(`loadCapability<Rule>(ruleCapability.id, { cwd })`, installed
`@oh-my-pi/pi-coding-agent@16.5.2` `capability/index.ts:228`) and passes the
fleet `Rule[]` concatenated ahead of the checkout's discovered rules —
fleet-first, both levels load, no cross-level dedup (`bucketRules` iterates in
array order, `capability/rule-buckets.ts:48-63`). This mirrors the CP-2
AGENTS.md compose and preserves the frozen "both levels load for rules"
precedence.

**Credential posture.** models.yml carries **two** credential-bearing
provider surfaces, not one: `apiKey` (`models-config.ts:23`; a configured
key is installed into the resolver, `model-registry.ts:1548-1552`) AND
`headers` (`models-config.ts:22`; header values materialize into outbound
request auth headers by `materializeConfigHeaderSources`,
`model-registry.ts:350-352`). Both run through the same `resolveConfigValue`
(`model-registry.ts:329-334`): **env-name-first, literal fallback** — a
value that is not an env-var name (or `!cmd`) resolves to itself. So a pinned
`providers.<name>.headers.Authorization: "Bearer sk-…"` (or any custom auth
header), exactly like a literal `apiKey`, is a bundle-borne credential. The
OQ-2 server-door denylist therefore covers **both**: the door
rejects a `models.yml` member whose parsed mapping sets
`providers.<name>.apiKey`, OR sets any `providers.<name>.headers.*` to a
**non-env-indirection literal** value (a value that is not an env reference
— i.e. a secret pinned in the file); an env-referenced header value passes,
so legitimate custom headers pinned to an env var cost operators nothing
(T1; client warning T5). Rationale over the CD-3-style out-of-scope
alternative: MCP configs are out of scope because credentialed MCP is
FORBIDDEN entirely (DL-080); models.yml IS delivered, so leaving either
credential surface unpoliced would re-open exactly the hole GC-5 closes for
settings. The wave's own file is credential-free (baseUrl/apiKey ride the
env per its own header), so the rule costs operators nothing today.

### The bundle-grammar and manifest delta (concrete)

**Bundle layout** (extends `config-reader.ts:10-17`'s layout):

```text
skills/<name>/…          # unchanged
extensions/<name>/…      # unchanged
mcp/<name>.json          # unchanged
settings/config.yml      # NEW — the fleet OMP settings document (YAML mapping; yml-only, OQ-1)
AGENTS.md                # NEW — the fleet context file (top-level regular file)
rules/<name>.md|.mdc     # NEW — fleet rules, flat regular files (CP-4)
agents/<name>.md         # NEW — fleet subagent definitions, flat regular files (CP-4)
models.yml               # NEW — fleet model routing/definitions (top-level regular file; CP-4)
version                  # unchanged (Runner-written)
```

Grammar deltas, held to the same door posture
(`go/internal/store/agent_config.go:104-108`: validation "runs BEFORE any row
write … rejected as a %w-wrapped ErrInvalidArgument"):

- `settings/` joins the top-dir whitelist in both validators
  (`agent_config.go:405-406`; `config_materialize.go:57-61` + `:442-443`),
  constrained to **exactly one regular file** `settings/config.yml`, whose
  content must parse as a YAML mapping (the door twin of the existing
  `mcp/*.json` "must parse as JSON" rule, `agent_config.go:107-108`) — a
  malformed fleet file would otherwise hard-crash every agent at boot via the
  strict overlay loader (installed `@16.5.2` `settings.ts:801-823`). The
  door check is best-effort (Go and Bun YAML parsers diverge); T4's
  container-side Bun-parse guard is the authoritative backstop.
- A top-level regular-file member named exactly `AGENTS.md` becomes legal
  (today `validateMemberPath` rejects any top-level non-directory:
  `config_materialize.go:447-449` "top-level %q must be a directory"; the
  store door has the same shape). No content validation beyond UTF-8/size (it
  is prose).
- `rules/` and `agents/` join the top-dir whitelist, each constrained to
  **flat regular files**: `rules/<name>.md` or `rules/<name>.mdc` (discovery
  globs exactly those extensions, flat, `builtin.ts:368-369` +
  `helpers.ts:476`), `agents/<name>.md` (subagent discovery accepts only
  `.md`, `task/discovery.ts:45`); `<name>` holds the existing member-name
  grammar. No content validation beyond UTF-8/size (markdown prose).
- A top-level regular file named exactly `models.yml` becomes legal
  alongside `AGENTS.md`; its content must parse as a YAML mapping (the same
  door twin as settings).
- The door additionally runs the **credential-key denylist** (OQ-2, decided
  (c)): a build step exports the SDK's `isCredential` key list
  (`settings-schema.ts:5497-5504`; currently 7 marked paths) into a
  generated Go slice, refreshed at fork bumps; a `settings/config.yml` that
  sets any denylisted path is rejected `ErrInvalidArgument`. For a
  `models.yml` member the door rejects any `providers.<name>.apiKey`, OR any
  `providers.<name>.headers.*` set to a non-env-indirection literal (a
  pinned secret; an env-referenced header value passes) — models.yml's two
  credential surfaces (CP-4). The door is authoritative: it survives a raw
  `PutAgentConfig` (`compass.connect.go:167`), which the client-side push
  warning (OQ-2 (b), T5) does not.

**Manifest delta** — `GetAgentConfigInfoResponse` today is
`{version=1, skills=2, extensions=3, mcp_servers=4}`
(`proto/compass/v1/compass.proto:554-563`), names-only by contract
(`compass.proto:552-553`: "names only, NEVER content"). Additive fields,
held-for-review (GC-2):

```proto
// Whether the bundle carries a fleet settings document (settings/config.yml).
bool has_settings = 5;
// Whether the bundle carries a fleet AGENTS.md.
bool has_agents_md = 6;
// Names of the bundle's rule files (rules/<name>.md|.mdc), names only.
repeated string rules = 7;
// Names of the bundle's subagent definitions (agents/<name>.md), names only.
repeated string subagents = 8;
// Whether the bundle carries a fleet models.yml.
bool has_models = 9;
```

Booleans for the singleton members (fixed names — a name list adds nothing,
and a content echo would break the names-only posture); name lists for the
two multi-member dirs, mirroring the existing skills/extensions/mcp shape
(`compass.proto:552-553`).

## Alternatives considered

### Curated key subset instead of whole-file passthrough (lost — Matt's ruling #1)

Deliver only a vetted allowlist (compaction, model roles, tool enablement) as
a Compass-owned schema. Lost because: (a) it creates a **second schema** that
must chase the SDK's `SETTINGS_SCHEMA` (5k+ lines,
`settings-schema.ts:383-5450`) on every fork bump — a permanent maintenance
tax; (b) the SDK already has a native, precedence-correct ingestion seam for
a whole file (the `configFiles` overlay list, `settings.ts:269`, consumed
via `Settings.loadIsolated`/`settingsManager`), so the subset buys no
safety the schema doesn't already provide; (c) TUI-only keys are inert in a
headless process — the cost of carrying them is zero. The one real risk of
whole-file (credential-marked keys) is a policy question (GC-5 + OQ-2's
decided (c)+(b) enforcement) rather than a schema-subset justification.

### AGENTS.md as workspace-placement (lost)

Have the Runner (or a provisioning step) write AGENTS.md into the checkout
root so the cwd walk-up finds it. Lost on three counts: (a) **collision** —
the walk-up collects the checkout's own AGENTS.md at the same path level
(`agents-md.ts:29`), so placement either overwrites repo content or needs a
merge policy in the Runner; (b) **ownership** — the checkout is the agent's
working tree; a Runner-written file shows up as dirty repo state the agent
may commit or delete; (c) **update path** — a workspace file is outside the
`current/` flip, so a ConfigVersion update would need a second write
mechanism, exactly the "second channel" this record's mandate forbids. The
`contextFiles` injection (CP-2 as amended; originally a user-level symlink)
gets composition, zero repo pollution, and Reload liveness for free.

### AGENTS.md as a `contextFiles` option override (originally lost; ADOPTED as amended, with manual composition)

Pass the mounted AGENTS.md via `createAgentSession({ contextFiles })`. The
frozen record rejected this because the option **replaces** discovery
(`sdk.ts:1177-1179` — provided ⇒ `Promise.resolve(options.contextFiles)`,
discovery skipped), which would silence the checkout's own AGENTS.md chain,
and merging both looked like re-implemented SDK logic with drift risk. The
2026-07-31 amendment adopts it: the installed runtime's session path loads
context files ONLY through the project-scope cwd walk-up
(`loadProjectContextFiles`, `sdk.ts:136`), so the user-level symlink the
record relied on was never read — and the drift objection dissolves because
the entrypoint composes by calling the SDK's own exported
`discoverContextFiles(cwd, agentDir)` (`sdk.ts:775-782`), no duplicated
walk-up, and prepending the fleet global (CP-2 as amended).

### Settings into the global `config.yml` write target (lost)

Materialize the fleet file at `$HOME/.omp/agent/config.yml`. Lost: that path
is the SDK's **persistence target** (installed `@16.5.2` `settings.ts:268`,
background saves `settings.ts:1322`); a copy forks fleet truth on first
write, a symlink into the read-only mount makes every settings write an
EROFS error. The overlay layer is read-only by design and carries the right
precedence (`settings.ts:1376-1378`).

### A fourth mount / separate delivery channel for settings (rejected outright)

A dedicated settings mount or a new RPC would duplicate the entire
store/fetch/materialize/Reload spine for one small file. Matt's ruling #3
already forecloses this; the bundle is the channel.

## Plan

Proto/door deltas are **held for review** (GC-2): named here, landed in the
implementation PR after `compass.v1`-owner review. All code lands in the
compass repo; paths are compass-repo-relative.

### T1 — Store door + manifest delta: settings, AGENTS.md, rules/, agents/, models.yml + the credential denylist

Extend the bundle grammar at the Server door and surface the new members in
the info RPC.

- Interfaces:
  - `go/internal/store/agent_config.go` — `configBundleTopDirs` gains
    `"settings"`, `"rules"`, `"agents"`;
    `configMemberParts`/`validateRegularMember` (`agent_config.go:389-449`)
    gain: `settings/` admits exactly the regular file `settings/config.yml`
    (yml-only — OQ-1; no `.yaml`/`.json` variant) whose bytes parse as a
    YAML mapping (reject scalars/arrays — the strict overlay loader's
    contract, installed `@16.5.2` `settings.ts:819-821`: "Config overlay
    must be a YAML mapping"); `rules/` admits flat regular files
    `rules/<name>.md`/`rules/<name>.mdc` and `agents/` flat regular files
    `agents/<name>.md`, each `<name>` grammar-valid; top-level regular files
    named exactly `AGENTS.md` or `models.yml` are admitted (any other
    top-level file stays rejected), `models.yml` YAML-mapping-validated. All
    remain inside the existing decompressed-size/file-count caps
    (`agent_config.go:58-62`).
  - **Credential denylist (OQ-2 (c), authoritative):** a new generated file
    `go/internal/store/credential_keys_gen.go` — a Go string slice of the
    SDK's credential-marked setting paths, exported by a codegen step from
    `isCredential` (`settings-schema.ts:5497-5504`), refreshed at fork bumps
    (the generation step joins the fork-bump checklist CP-3's prevention
    property names). The door walks the parsed `settings/config.yml` mapping
    and rejects any set denylisted path; for `models.yml` it rejects any
    `providers.<name>.apiKey`, and any `providers.<name>.headers.*` whose
    value is a non-env-indirection literal (a pinned secret — an
    env-referenced value passes; models.yml's two credential surfaces per
    CP-4). Each rejects `ErrInvalidArgument` naming the offending key path.
  - `configBundleMemberNames` (`agent_config.go:197`) additionally returns
    `hasSettings, hasAgentsMD, hasModels bool` and
    `rules, subagents []string`.
  - `proto/compass/v1/compass.proto` — `GetAgentConfigInfoResponse` gains
    `bool has_settings = 5; bool has_agents_md = 6; repeated string rules = 7;
    repeated string subagents = 8; bool has_models = 9;` (additive,
    buf-breaking-clean); handler populates them from
    `configBundleMemberNames`.
  - Version semantics unchanged: the canonical content hash
    (`compass.proto:544-546`) already covers the new members' paths+bytes, so
    adding any member mints a new version and drives the existing
    `ConfigVersion` fan-out with zero signal changes.
- Consumes: landed store (`agent_config.go`, `0008_agent_config.sql`).
  Produces: a door that admits the five delivered members; info RPC
  visibility; the authoritative credential gate.
- Acceptance: table-driven door tests extend
  `agent_config_test.go` — `settings/config.yml` with a YAML mapping passes;
  `settings/config.yaml`, `settings/config.json`, `settings/other.yml`,
  nested `settings/a/b.yml`, and non-mapping YAML are each rejected
  `ErrInvalidArgument`; `rules/a.md`, `rules/b.mdc`, `agents/design.md`, and
  a top-level `models.yml` mapping pass; `rules/nested/a.md`, `agents/a.txt`,
  and a top-level file other than `AGENTS.md`/`models.yml` are rejected; a
  settings file setting a denylisted key (e.g. `auth.broker.token`), a
  `models.yml` setting `providers.x.apiKey`, and a `models.yml` setting
  `providers.x.headers.Authorization` to a literal `"Bearer …"` are each
  rejected with the key path named (while `providers.x.headers.X-Org` set to
  an env reference passes); `GetAgentConfigInfo` reports all five new fields.
  Suggested tier: sonnet.

### T2 — Runner unpack whitelist delta (defense-in-depth twin of T1)

Mirror the grammar in the Runner's `validateAndUnpack`, which re-enforces
every guard at unpack (`config_materialize.go:324-326`).

- Interfaces:
  - `go/internal/runner/config_materialize.go` — `configTopDirs`
    (`:57-61`) gains `"settings"`, `"rules"`, `"agents"`;
    `validateMemberPath` (`:424`) admits the top-level regular files
    `AGENTS.md` and `models.yml`, constrains `settings/` to
    `settings/config.yml` (yml-only), `rules/` to flat `<name>.md`/`<name>.mdc`,
    and `agents/` to flat `<name>.md`; the mcp-style content check
    (`:397-400`) gains the YAML-mapping check for `settings/config.yml` and
    `models.yml` — a **NEW direct YAML dependency** (`go.mod` has none
    today: require block `go.mod:17-26`); the implementation PR chooses the
    lib; `gopkg.in/yaml.v3` is archived/unmaintained — consider
    `goccy/go-yaml`. The credential denylist (T1) is NOT mirrored here —
    the store door is the authoritative credential gate; the Runner
    re-enforces structure, not policy. T4's container-side Bun-parse guard
    makes the Go-side YAML check a best-effort door nicety, whichever lib.
  - No change to `Materialize`, `flipCurrent`, `prune`, `RefreshConfig`, or
    the mount append (`host.go:168`) — the members are ordinary tree content.
- Consumes: T1's grammar (kept textually in lockstep). Produces: a Runner
  that unpacks the new members into `<root>/<version>/`.
- Acceptance: `config_materialize_test.go` cases — a bundle carrying all
  five new members lands them under the version dir with pinned modes; T1's
  structural rejection matrix reds at unpack (the credential denylist is
  store-door-only, not mirrored). Suggested tier: sonnet.

### T3 — In-container reader: settings, AGENTS.md, models.yml, rules/, agents/ accessors

Extend `config-reader.ts` with the new tolerant readers, mirroring the
module's absent→empty posture (`config-reader.ts:19-23`).

- Interfaces:
  - `export async function readMountedSettingsPath(currentDir: string): Promise<string | undefined>`
    — the absolute path of `<currentDir>/settings/config.yml` when it exists
    and is a regular file, else `undefined`. Existence only — parsing is the
    SDK's job (strict, by design).
  - `export async function readMountedAgentsMd(currentDir: string): Promise<{ path: string; content: string } | undefined>`
    — the fleet `AGENTS.md` read as a `{ path, content }` object for direct
    `contextFiles` injection (CP-2 as amended); absent/unreadable ⇒
    `undefined` (tolerant).
  - `export async function readMountedModelsPath(currentDir: string): Promise<string | undefined>`
    — the path contract for `<currentDir>/models.yml`.
  - `export async function readMountedRules(currentDir: string): Promise<Rule[]>`
    — the mounted `rules/*.md`/`*.mdc` built into `Rule[]` with the SDK's own
    `createSourceMeta` + `buildRuleFromMarkdown` (CP-4 as amended); absent
    dir ⇒ `[]`, an unreadable file skipped, never fatal. And
    `readMountedAgentsDir(currentDir): Promise<string | undefined>` — the
    absolute path of `<currentDir>/agents` when it exists and is a
    directory, else `undefined` (content loading is the SDK's job through
    the CP-4 symlink).
  - `MountedConfig` (`config-reader.ts:281-287`) gains
    `settingsPath?: string`, `agentsMd?: { path, content }`,
    `modelsPath?: string`, `rules: Rule[]`, and `agentsDir?: string`,
    populated by `loadMountedConfig`.
- Consumes: the mount contract (T2's on-disk layout). Produces: the fields T4
  wires.
- Acceptance: `config-reader.test.ts` tempdir fixtures — present, absent,
  dir-instead-of-file (and file-instead-of-dir for rules/agents) cases;
  `loadMountedConfig` carries all five fields; no reader ever throws.
  Suggested tier: sonnet.

### T4 — Entrypoint wiring: settings/rules/AGENTS.md object injection + the agents/models.yml symlinks (CP-1/CP-2/CP-4)

The last mile in `main()` (`cli.ts:301`), before `createAgentSession`.

- Interfaces:
  - Settings — **object injection, parse-guarded, fail-open.** When
    `mounted.settingsPath` is set, first try Bun's `YAML.parse` on the
    member (the same parser the strict overlay loader uses,
    `settings.ts:814`); on success build the fleet instance with
    `Settings.loadIsolated({ cwd, agentDir, configFiles: [mounted.settingsPath] })`
    — **NOT `Settings.init`**, which caches a global singleton and returns
    it unchanged on any later call (`settings.ts:289-290`: "`if
    (globalInstancePromise) return globalInstancePromise;`"), silently
    dropping our `configFiles` — and pass it to `createAgentSession` as
    `settingsManager` (resolution seam `sdk.ts:1154-1156`; wired into the
    capability layer by `initializeWithSettings`, `capability/index.ts:251`).
    On parse failure log loudly and pass NO `settingsManager` — **fail-open
    to SDK defaults** (OQ-7, decided: fail-open with loud logging, Matt
    2026-08-03). The guard still closes the parser-divergence class: the
    door parses with Go YAML (T1/T2) while the agent parses with Bun's
    `YAML.parse`, and a file the door accepts but Bun rejects would
    otherwise hard-crash every agent via the strict overlay loader
    (`settings.ts:801-823`) — a crash the update path cannot see (Reload
    "success" = `StartAgent` spawning, `host.go:467-474`; no crash-loop
    supervision on the agent exec), leaving the fleet dead until an operator
    pushes a fixed bundle. When `mounted.settingsPath` is **unset**, pass
    nothing: the SDK falls through to its own
    `Settings.init({ cwd, agentDir })` (`sdk.ts:1154-1156`) — unconfigured ⇒
    SDK defaults by construction (no env channel exists to clear).
  - Rules — **object injection, COMPOSED.** Build the fleet `Rule[]` from the
    mounted `rules/*.md`/`*.mdc` with the SDK's own `createSourceMeta(provider,
    filePath, "user")` (`discovery/helpers.ts:124`) +
    `buildRuleFromMarkdown(name, content, filePath, source,
    { stripNamePattern: /\.(md|mdc)$/ })` (`discovery/helpers.d.ts:94`).
    Because providing `rules:` short-circuits SDK rule discovery
    (`sdk.ts:1434-1436`), re-run that discovery explicitly
    (`loadCapability<Rule>(ruleCapability.id, { cwd })`, `capability/index.ts:228`)
    and pass `[...fleetRules, ...discovered]` unconditionally (empty fleet set
    still composes) — fleet-first, both levels load, mirroring the AGENTS.md
    compose below.
  - AGENTS.md — **object injection, COMPOSED.** When `mounted.agentsMd` is
    present (read as `{ path, content }`), pass `contextFiles:
    [mounted.agentsMd, ...(await discoverContextFiles(cwd, agentDir))]` —
    fleet global FIRST (least prominent, the user-level position), the
    entrypoint re-running the SDK's own project walk-up itself because
    providing the option short-circuits discovery (`sdk.ts:1177-1179`).
    When absent, OMIT the key so native project discovery runs.
  - Agent-dir symlinks (CP-4, the two no-object-seam members): a single
    idempotent
    `ensureAgentDirLink(home: string, entry: string, target: string | undefined): Promise<void>`
    (exported, pure-decision-testable per the cli.ts structure note,
    `cli.ts:25-29`), applied to the two entries `agents` and `models.yml` —
    target set: `mkdir -p $HOME/.omp/agent`; when `$HOME/.omp/agent/<entry>`
    is absent or an existing **symlink**, (re)point it at the
    **`current/`-relative** mount path
    `/run/compass/agent-config/current/<entry>` (constant, not the resolved
    version dir — the flip-liveness point); a pre-existing **regular file or
    real directory** at that path is never clobbered — log and leave it in
    place (it wins). Nothing writes there today (grep of
    `go/internal/runner` for `.omp/agent|AGENTS.md` is empty; only
    `$HOME/.compass/*` is written, `cli.ts:13-16,67-69`), so the guard is
    symmetric with the removal path. Target unset: remove a Compass-owned
    symlink if present (never a regular file or dir a user placed). Failures
    log and continue (tolerant boot).
  - All of it runs after `loadMountedConfig` and before the
    `createAgentSession` call (`cli.ts:381-395` ordering).
  - `MainDeps` needs no new members — every effect is real-FS over the
    injectable `configMount` (`cli.ts:382`).
- Consumes: T3. Produces: an agent whose session runs on the fleet Settings
  overlay (`settingsManager`), the composed fleet+project rules (`rules[]`),
  and the composed fleet+project context files (`contextFiles`), with subagent
  definitions
  and models.yml found natively through the two remaining agent-dir
  symlinks.
- Acceptance: `cli.test.ts` over the `MainDeps` seam with a tempdir mount —
  (a) settings member present + parseable ⇒ the session's Settings resolve an
  overlay-set key over a project-layer value; (b) absent ⇒ no
  `settingsManager` passed and boot is clean on SDK defaults; (c) member
  present but Bun-unparseable ⇒ no `settingsManager`, loud log, boot clean
  on SDK defaults; (d) a fleet AGENTS.md composes with the checkout's
  project AGENTS.md — both present in `contextFiles`, fleet first; absent ⇒
  the key is omitted; (e) a fleet rule and the checkout's own discovered rule
  both land in `rules[]` (frontmatter parsed), fleet-first, and the array is
  passed even when the fleet set is empty; (f) each of the two
  symlink entries (`agents`, `models.yml`) present ⇒ its symlink exists and
  resolves through `current/`; removed on a re-run ⇒ link removed; a
  pre-existing regular file (or real dir) survives both create and remove
  paths, logged; (g) with the agents link in place, SDK discovery resolves a
  mounted subagent def by name. Suggested tier: opus (the ordering +
  tolerance interplay).

### T5 — Operator CLI surface: push/show carry the new members

The push verb tars a directory verbatim, so the delta is
validation-mirroring, the OQ-2 (b) credential warning, and display.

- Interfaces:
  - `compass agent-config push <dir>` (sibling T2's verb) accepts a source
    dir containing any of `settings/config.yml`, `AGENTS.md`, `rules/`,
    `agents/`, and `models.yml`; any client-side pre-validation mirrors T1's
    grammar (server door remains authoritative).
  - **Credential warning (OQ-2 (b)):** push scans `settings/config.yml`
    against the same generated denylist key set, and `models.yml` for
    `providers.*.apiKey` plus any `providers.*.headers.*` set to a literal
    (non-env-indirection) value, printing a loud warning naming each
    offending key BEFORE the upload attempt — early operator feedback; the
    server door is the gate that actually rejects.
  - `compass agent-config show` renders `has_settings` / `has_agents_md` /
    `has_models` and the `rules` / `subagents` name lists from the extended
    `GetAgentConfigInfoResponse`.
- Consumes: T1. Produces: the operator workflow (the config-repo + CI `put`
  flow DL-078 names) carrying the new members with no workflow change.
- Acceptance: CLI test — a dir with all five members pushes and `show`
  reports them; a dir with `settings/extra.yml` fails with the door's field
  error; a credentialed settings or models file triggers the client warning
  and the door rejection. Suggested tier: sonnet.

### T6 — End-to-end + Reload coverage

No new machinery — pin that the existing update path carries the new
categories.

- Interfaces: none new. Extends the landed fixtures:
  `config_refresh_test.go` (RefreshConfig fan-out,
  `config_refresh_test.go:5-9`) and the compass-agent integration tests.
- Coverage:
  - Provision with a bundle carrying
    settings+AGENTS.md+rules+agents+models.yml ⇒ container boots with all
    applied (T4's acceptance, driven end-to-end): the agent's Settings
    resolve the overlay, a mounted rule is injected, a mounted subagent
    def resolves by name, and the ModelRegistry loads the mounted models.yml
    (an override key observable on the registry).
  - `PutAgentConfig` with a changed `settings/config.yml` (or `models.yml`,
    or a rule file) ⇒ new content hash ⇒ `ConfigVersion` ⇒ `RefreshConfig`
    re-materializes + Reloads (`host.go:588`, version-moved gate
    `host.go:70-74`) ⇒ the re-exec'd agent observes the new content through
    every seam — rebuilt objects and the two symlinks — no container restart.
  - Unconfigured / partially-configured bundles (skills only, settings only,
    rules only) keep the tolerant-empty contract.
- Acceptance: both suites green; the Reload case asserts on the *agent's
  observed* setting value, not just the flipped symlink. Suggested tier:
  opus.

## Tasks

- [ ] T1 — Store door + `GetAgentConfigInfoResponse` delta: admit
      `settings/config.yml` (yml-only, YAML-mapping-validated), top-level
      `AGENTS.md` + `models.yml`, flat `rules/` + `agents/` members; the
      generated credential-key denylist door check (settings paths +
      models.yml provider apiKey); `has_settings`/`has_agents_md`/
      `has_models` + `rules`/`subagents` fields (held-for-review)
- [ ] T2 — Runner `validateAndUnpack`/`validateMemberPath` whitelist delta
      (defense-in-depth twin of T1; structure only — the denylist stays at
      the store door)
- [ ] T3 — `config-reader.ts`: `readMountedSettingsPath` /
      `readMountedAgentsMd` (`{path, content}`) / `readMountedModelsPath` /
      `readMountedRules` (`Rule[]`) / `readMountedAgentsDir` +
      `MountedConfig` fields
- [ ] T4 — `cli.ts main()`: settings via a Bun-parse-guarded
      `Settings.loadIsolated` `settingsManager` (fail-open per OQ-7; absent
      ⇒ none passed ⇒ SDK defaults), rules via a composed `rules[]`
      (`[...fleetRules, ...discovered]`, fleet-first),
      fleet AGENTS.md via composed `contextFiles` (omitted when absent);
      idempotent, never-clobbering `ensureAgentDirLink` for `agents` +
      `models.yml` → `current/<entry>` symlinks only
- [ ] T5 — `compass agent-config push/show`: carry + display the new
      members; client-side credential warning (OQ-2 (b))
- [ ] T6 — E2E + Reload coverage: provision-time application and
      ConfigVersion-driven in-place update of all five delivered categories

## Open Questions

OQ-1, OQ-2, OQ-6, and OQ-7 are **RESOLVED** (Matt, 2026-08-03) and kept
below as settled calls; OQ-3/OQ-4/OQ-5 remain open, assumption-designed per
the batching rule — none blocks the Plan's shape.

1. **RESOLVED (Matt, 2026-08-03) — the settings member is
   `settings/config.yml`, YAML-only.** Ruling: "yml, and only yml so it's
   simple." The bundle member is exactly `settings/config.yml`;
   `.yaml`/`.json` variants and the author-as-JSON latitude are dropped
   (T1/T2 grammar; CP-1 prose updated). (Original question: Matt's ruling
   said "config.json" while the SDK's canonical settings document is
   `config.yml` — `MAIN_CONFIG_FILENAMES`, `dirs.ts:25-26`.)
2. **RESOLVED (Matt, 2026-08-03) — credential enforcement is (c)+(b).**
   Ruling: a **generated credential-key denylist at the server door** (a
   build step exports the `isCredential` list,
   `settings-schema.ts:5497-5504` — the `isCredential`-marked paths
   (currently 7; the prose need not enumerate them, the codegen is the
   source of truth) into a generated Go slice, refreshed at fork bumps; T1)
   PLUS a client-side
   warning in `compass agent-config push` (T5). The server door is
   authoritative — it survives a raw `PutAgentConfig`
   (`compass.connect.go:167`); the client warning is early feedback only.
   Extended to the delivered models.yml, which has **two** credential
   surfaces: the door rejects `providers.<name>.apiKey` AND any
   `providers.<name>.headers.*` set to a non-env-indirection literal (a
   pinned secret; an env-referenced header value passes) — CP-4's credential
   posture. (Refined 2026-08-03 after review found provider `headers` values
   materialize into outbound auth, `model-registry.ts:350-352`; an
   apiKey-only gate would leave a second literal-credential path in the
   delivered file.)
3. **(Non-load-bearing) `COMPASS_MODEL` vs fleet `modelRoles` precedence.**
   **Assumption:** the Runner's `COMPASS_MODEL` → `modelPattern`
   (`cli.ts:397`) stays authoritative for the main model; fleet `modelRoles`
   govern role-resolved models (compaction/subagent). Whether fleet model-role
   *names* resolve against the container's available providers is a
   deployment-content question (the auth seed defines what's usable), not a
   mechanism one.
4. **(Non-load-bearing) Additive AGENTS.md composition.** **Assumption:** the
   fleet AGENTS.md (global) and the checkout's own AGENTS.md (project) SHOULD
   both apply — as amended, composed by the entrypoint (`[fleetGlobal,
   ...discoverContextFiles(cwd, agentDir)]`). If Matt wants fleet-exclusive,
   the change is dropping the re-run discovery and passing `[fleetGlobal]`
   alone, gated on a flag — named, unbuilt.
5. **(Non-load-bearing) TUI-only keys in a headless process.** Settings hooks
   fire at init (theme/symbols, `settings.ts:38`, `:1053` `#fireAllHooks`) —
   **assumption:** inert without a TUI, as the existing headless
   `createAgentSession` path already initializes Settings with defaults today.
   T6's boot test is the guard.
6. **RESOLVED (Matt, 2026-08-03) — the defer set shrinks to what the wave
   doesn't use.** Ruling: "need to support everything we currently use in
   this wave for MVP." The MVP delivers every category the current wave
   populates — adding rules/, subagent definitions (agents/), and models.yml
   to settings + AGENTS.md (CP-4; the CP-3 table updated). Categories the
   wave does not populate (commands, prompts, instructions, hooks, custom
   tools, watchdog files, the `ConfigFile` family, legacy settings.json,
   plugin-overrides.json, LSP/DAP) stay deferred by name.
7. **RESOLVED (Matt, 2026-08-03) — fail-open.** Ruling: fail-open with loud
   logging, the T4 guard exactly as specced — a member the door accepted but
   Bun rejects is logged loudly and ignored, and the agent boots on
   SDK-default settings rather than crash-looping invisibly at the strict
   overlay load (`settings.ts:801-823` in the installed 16.5.2; no
   crash-loop supervision on
   the agent exec, `host.go:467-474`, so a fail-closed crash would leave the
   fleet dead until an operator pushes a fixed bundle).
