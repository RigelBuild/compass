# Compass per-Manager profiles and in-process subagent topology

Status: Active

Refs: RIG-2936 (this record). Sibling: RIG-2935 (harness/OMP profile lane —
FROZEN 2026-08-29, owns the shared profile concept, registry discipline, and
selector grammar this record adopts; its cross-lane contract is the
`## Shared override schema` section of the frozen RIG-2935 record and ledger
rows DL-062/063/064). RIG-2937 (subtree composition authority) — RULED YES,
unconditional; see Resolved decisions. Composes with: RIG-1715 / PR #671 (the
LLM gateway, merged — frozen record
`docs/designs/server/compass-server-llm-gateway/design.md` — the single LLM
egress the profile's model axis resolves through), RIG-2845 (Compass model
roles + stable-name provider routing — the policy layer the profile's model
fields reference), RIG-1716 (embedded MCP gateway — where the deferred
extensions/MCPs axis will resolve). Builds on: RIG-2673
(`compass-agent-org-mgmt-tools/design.md`), the frozen spawn/despawn record
(`compass-agent-spawn-despawn/design.md`), and the model-eval suite
(RIG-2562) whose per-role recommendations this rollout mechanism deploys.

## Problem / Intent

Naming note: the record path and issue title ("per-agent overrides") describe
the SUPERSEDED per-spawn shape; the design here is the profile system. The
path is kept to avoid churning the ledger and every cross-reference.

The model-eval suite (RIG-2562) produces per-role model recommendations
(Manager / implement / design / design-critic / review, each at a chosen
thinking level), but Compass cannot deploy them per Manager: the model is
Runner-global (`runner.go:48-51`), and there is no named, version-controlled
unit of agent configuration a Manager can be pinned to. This record introduces
the **profile** — a named, VC'd config bundle set per MANAGER — and the
**in-process subagent topology** that makes "worker" tasks OMP subagents
inside the Manager's container rather than peer containers. Together they let
eval output roll out incrementally (one Manager's tree onto a candidate
profile while others stay on the known-good default), with the profile
reaching the Manager's whole subagent subtree through the SDK's own
settings-propagation spine.

The eval→rollout handoff is by NAME: the eval suite (RIG-2562) emits a profile
NAME per run as its rollout artifact (§T10), and deploying a recommendation
means pinning a Manager's tree to that named profile — never copying model
strings around.

## Approach

### Grounding: what is per-agent today, and what is not

- **Model is Runner-global.** `AgentModel` is "the model selector handed to
  every agent this Runner starts (the agent's COMPASS_MODEL)"
  (`go/internal/runner/runner.go:48-51`), threaded to
  `AgentHostConfig.AgentModel` (`go/internal/runner/host.go:118-120`) and
  applied identically to every container: "The model is Runner-wide config;
  everything else is per-container" (`host.go:875-877`), via `agentEnv`:

  ```go
  // go/internal/runner/host.go:878-887
  func (h *agentHost) agentEnv(handle *runtime.AgentHandle) AgentEnv {
      return AgentEnv{
          UID:     handle.WorkspaceUID(),
          HomeDir: handle.HomeDir(),
          Workdir: handle.CheckoutDir(),
          Model:   h.model,
          Persona: handle.Persona(),
          Role:    handle.Role(),
      }
  }
  ```

  The exec injects it omitted-when-empty: `if e.Model != "" {
  spec.Env["COMPASS_MODEL"] = e.Model }`
  (`go/internal/runner/agent_exec.go:83-85`), and the in-container entrypoint
  reads it opaquely — `resolveModelSelector(env)` returns
  `env.COMPASS_MODEL?.trim()` "as an opaque pattern string for
  `createAgentSession` to resolve against its own model registry"
  (`packages/compass-agent/src/cli.ts:136-150`), forwarded as `modelPattern`
  (`cli.ts:865-869`).

- **Role and persona are per-agent but server-authoritative.**
  `ProvisionAgentWorkspaceRequest.persona`/`.role` are documented
  "SERVER-AUTHORITATIVE: the Server is expected to populate this by reading
  AgentAccount.role from the store on the provision path and to overwrite any
  client-supplied value, so a caller cannot inject a role prompt"
  (`proto/compass/v1/compass.proto:589-600`; persona at `:579-588`). The
  overwrite is live code on the operator provision path:

  ```go
  // go/server/service.go:159-165
  if acc.IsAgent() {
      req.Msg.Persona = acc.Agent.Persona
      req.Msg.Role = acc.Agent.Role
  } else {
      req.Msg.Persona = ""
      req.Msg.Role = ""
  }
  ```

  On the agent-initiated spawn path the values are hardcoded empty at
  creation — "Persona and role are server-authoritative and empty on spawn"
  with `Persona: "", Role: ""` in the `store.CreateAgent` literal
  (`go/server/lifecycle.go:180-194`) — then threaded from the **created store
  account**, never the request: `l.provisionAndStart(ctx, created.ID,
  created.Agent.Persona, created.Agent.Role, req)` (`lifecycle.go:197`).

- **The agent-facing spawn tool cannot configure a peer.** The shipped
  `SpawnPeerRequest` carries only `handle = 1`, `display_name = 2`,
  `reserved 3; reserved "initial_prompt";`, `client_request_id = 4`
  (`proto/compass/v1/agent_gateway.proto:167-173`). Field 3
  (`initial_prompt`) is **reserved, not reclaimable** (DL-187); RIG-2673
  claims `role = 5` / `persona = 6`
  (`compass-agent-org-mgmt-tools/design.md:107-133`). New fields take fresh
  numbers.

- **The config mount is the delivery vehicle for versioned config.** Bundles
  flow store door → Runner `ConfigMaterializer` unpack → read-only container
  mount with atomic `current` flips: "The container mounts the PARENT dir
  (root) read-only, so an atomic flip of `current` becomes visible inside the
  running container without a remount"
  (`go/internal/runner/config_materialize.go:3-11`). The store door admits a
  closed top-dir whitelist — `skills`, `extensions`, `mcp`, `settings`,
  `rules`, `agents` plus exact-name top-level files `AGENTS.md`/`models.yml`
  ("the set is closed at the store door, not filtered downstream",
  `go/internal/store/agent_config.go:31-53`). In-container, the role's
  block-0 comes from `prompts/<role>/SYSTEM.md` read by
  `readMountedRolePrompt` (traversal-guarded: "Reject a separator or `..` so
  the label can never traverse outside the `prompts/` subtree",
  `packages/compass-agent/src/config-reader.ts:370-390`), injected as
  `customSystemPrompt` (`cli.ts:940`); fleet rules load from `rules/`
  (`config-reader.ts:413`); `models.yml` is symlinked into
  `$HOME/.omp/agent/models.yml` for the SDK ModelRegistry
  (`config-reader.ts:397-401`, `cli.ts:789`); `settings/config.yml` becomes a
  `Settings` overlay via `buildFleetSettings` (`cli.ts:497`, applied
  `cli.ts:782-785`).
  Delivery-gap caution: `prompts/` is in NONE of the bundle whitelists — the
  store door (`agent_config.go:35-42`), the runner unpack
  (`config_materialize.go:71-78`), and the CLI bundle builder
  (`bundle.go:63-70`) all enumerate only the six dirs above — so the bundle
  pipeline cannot deliver a `prompts/` tree today; the in-container reader
  exists, but its mount content arrives out-of-band. The whitelisted dirs a
  new top-dir is genuinely analogous to are `rules/` and `agents/` (T1 must
  not replicate the `prompts/` gap).

- **The SDK already resolves per-subagent models from parent settings.** On
  Compass's pinned SDK 16.5.2, the task tool resolves a subagent's model as
  "per-agent model override from settings (highest priority)":

  ```ts
  // @oh-my-pi/pi-coding-agent@16.5.2 src/task/index.ts:1271-1281
  const agentModelOverrides = this.session.settings.get("task.agentModelOverrides");
  const settingsModelOverride = agentModelOverrides[agentName];
  const parentActiveModelPattern = this.session.getActiveModelString?.();
  const modelOverride = resolveAgentModelPatterns({
    settingsOverride: settingsModelOverride,
    ...
  ```

  with auth-aware fallback to the parent's active model in
  `resolveModelOverrideWithAuthFallback`
  (`src/config/model-resolver.ts:1261-1297`) — a silent substitution Compass
  REJECTS by ruling (see Resolved decisions). A profile rendered into the
  Manager session's settings therefore covers the Manager AND every subagent
  it spawns, with no per-spawn plumbing.

### The profile: a named, VC'd config bundle, set per Manager

The unit of configuration is the **profile** — a named, version-controlled
config bundle in a registry that rides the existing store-door → mount
pipeline. **Every Compass agent has a profile — there is no non-profile
agent** (the store column defaults to `'default'`), and the committed fleet
config IS the `default` profile, which must always exist and resolve. The
concept, the registry discipline, and the selector grammar are
adopted from the harness lane (RIG-2935); the SCHEMA is not — the harness
profile is deliberately model-stack-scoped (a sparse modelRoles diff), while
**Compass designs its own superset from scratch** (Matt's explicit call —
Compass owns much more of agent/config delivery than the harness does).

The full superset schema, versioned as `profiles/<name>/profile.yml` in the
fleet config bundle:

```yaml
# profiles/<name>/profile.yml — the Compass profile superset (v1 consumes
# ONLY the `models` axis; the rest is schema'd, consumption deferred, and
# deferred-axis shapes are PROVISIONAL until first consumption — see
# §Resolved decisions).
models:
  # The Manager's own model. Semantic `manager` key, shared with the harness
  # profile schema for cross-lane alignment (see §Shared anchors).
  manager: litellm/claude-opus:high
  # Per-subagent-role models, keyed by SDK agent name. Rendered into the
  # session settings as `task.agentModelOverrides` (the SDK's own highest-
  # priority per-agent resolution, task/index.ts:1271-1281). Keys are
  # statically linted at the door against the FRONTMATTER `name:` of each
  # shipped agents/*.md def in the SAME bundle — the SDK resolves a subagent
  # by `agent.name` (`getAgent` task/discovery.ts:143-144;
  # `parseAgentFields` requires frontmatter `name`, helpers.ts:242), NOT the
  # filename stem — so a key matching no def name is a SILENT no-op at spawn
  # time; the lint turns a typo (e.g. `implement` vs the shipped def whose
  # frontmatter is `name: implementer`) into a reviewable door failure.
  agents:
    implementer: litellm/claude-sonnet:medium
corpus:
  # System-prompt corpus selection (role-prompt set, skills, rules).
  # SCHEMA'd, consumption DEFERRED — a fresh Compass-owned axis, NOT a
  # reopening of the harness instruction-slice cut (see OQ-6 reconciliation
  # in Resolved decisions).
  prompts: null
  skills: []
  rules: []
extensions:
  # Extension/MCP tool-set selection. SCHEMA'd, consumption DEFERRED; will
  # resolve against the RIG-1716 embedded MCP gateway (a tool-set/ACL ref,
  # never per-container MCP creds) when that record lands.
  mcp: null
settings:
  # Session-settings overlay (compaction thresholds etc), same key grammar
  # as settings/config.yml. SCHEMA'd, consumption DEFERRED.
  {}
```

- **v1 scope ruling: full superset SCHEMA, model-stack SHIPS.** v1 populates
  and consumes only `models` — the eval-driven core and the shared anchor
  with the harness. `corpus`/`extensions`/`settings` are schema'd so later
  phases grow additively with no schema break. This unblocks the RIG-2562
  eval rollout fast.
- **A profile is set per MANAGER.** The Manager's own model and the models
  of every subagent role in its tree all come from its profile. Absent/empty
  profile = the `default` profile, whose contents are the committed fleet
  config — no behavior change for unpinned Managers (adopting the harness
  "the committed config IS the `default` profile" discipline).
- **Profile change = teardown + re-provision + relaunch with resumed
  session.** A Manager changing profile (or its profile being updated, e.g.
  the operator adjusts `default`) is applied by tearing the container DOWN
  and RE-PROVISIONING it, so the Runner rebuilds a fresh `AgentHandle` from
  the updated store row, then starting with resumed session context. A plain
  `Reload`/`RefreshConfig` is INSUFFICIENT: `reloadLocked` re-resolves the
  handle from the registry and derives env from the provision-captured
  handle spec (`host.go:857`, `agentEnv` `host.go:878`), so it relaunches
  with the STALE profile — the store write would no-op (see Resolved
  decisions; T4 specifies the orchestration; this promotes the old record's
  OQ-5 deferral to the chosen mechanism). No live mid-session profile
  mutation exists.
- **The registry rides the existing pipeline — with all THREE whitelists
  extended.** `profiles/` becomes a new whitelisted top-dir at every layer
  that enumerates the bundle's top dirs: the store door
  (`agent_config.go:35-42`, `configBundleTopDirs` `:57-64`), the runner
  unpack (`config_materialize.go:71-78`), and the CLI bundle builder
  (`bundle.go:63-70`), plus each layer's per-member grammar arm (T1). A
  bundle admitted at only one layer fails the whole fleet unpack. The
  registry is the durable source; the mount is its durable per-container
  materialization — never a self-reaping tmpfile (see §Global Constraints,
  overlay lifetime).

### Shared anchors with the harness (RIG-2935)

Byte-aligned across both lanes; everything else is per-lane:

- **Selector grammar.** A model selector splits on the LAST colon; the
  thinking suffix must be a level in the model's effort ladder in
  `models.yml`. Outside that split, the selector is opaque end to end — the
  Compass entrypoint "deliberately does not parse provider/id itself"
  (`cli.ts:138-140`), and no Compass component splits provider/model.
- **Family is explicit reviewable data, not name-parsing.** A model's family
  is registry data on its `models.yml` entry; the cross-family review
  constraint (§Cross-family review constraint) computes from that data,
  never from parsing selector strings.
- **The semantic `manager` key.** OMP cannot rename its base role ("default"
  is hardcoded at ~10 SDK sites), so the portable profile schema uses a
  semantic `manager` key that the OMP launcher translates to
  `modelRoles.default`. Compass uses the SAME `manager` key in its `models`
  axis — Matt picked the key for exactly this cross-lane alignment. An eval
  rollout artifact (role → model map keyed `manager`/agent-role) is
  consumable by both lanes without translation.
- **NOT shared: the schema.** OMP's profile stays a model-stack-scoped
  sparse modelRoles diff; the Compass superset above is Compass-owned.

### Propagation spine: profile → Manager session → subagent subtree

The profile reaches the whole tree through two hops, both existing seams:

1. **Store → provision → runner → container.** The agent's account carries a
   `profile` name (a bounded selection, like `role`), threaded
   store → `ProvisionAgentWorkspaceRequest` → `AgentSpec` → container env as
   `COMPASS_PROFILE`, following exactly the persona/role pipeline so
   store-as-source-of-record is preserved (`service.go:159-165` overwrite
   shape; `lifecycle.go:197` threading shape). The env var carries only the
   NAME; the contents come from the mounted registry.
2. **Entrypoint render.** The entrypoint resolves `COMPASS_PROFILE` against
   the mounted `profiles/<name>/profile.yml` (traversal-guarded like
   `readMountedRolePrompt`, `config-reader.ts:374-381`) and renders the v1
   model-stack: `models.manager` becomes the session `modelPattern`
   (the sole model source for a Compass-provisioned session, superseding the
   Runner-global `COMPASS_MODEL`), and
   `models.agents` becomes a `task.agentModelOverrides` record on the
   session's settings overlay — the same overlay layer `buildFleetSettings`
   already builds (`cli.ts:497`, `cli.ts:782-785`). From there the SDK's own
   resolution takes over (`task/index.ts:1271-1281`), so every subagent the
   Manager spawns resolves its model from the profile with zero per-spawn
   plumbing. The SDK's auth-fallback
   (`resolveModelOverrideWithAuthFallback`, `model-resolver.ts:1261-1297`)
   SILENTLY substitutes the parent's active model when the profile-named
   model fails its pre-flight auth check — that silent substitution is
   REJECTED by ruling: a named model that fails auth/registry resolution
   fails LOUD on ALL sessions, and Compass never relies on the SDK's
   built-in fallback (see Resolved decisions).

**Cross-pin divergence — snapshot, not live reference.** On harness 17.3.1
the subagent shares the parent Settings object BY REFERENCE, so a settings
change propagates live to arbitrary depth. On Compass's pinned 16.5.2,
`createSubagentSettings` takes a SNAPSHOT COPY per key:

```ts
// @oh-my-pi/pi-coding-agent@16.5.2 src/task/executor.ts:768-776
export function createSubagentSettings(
  baseSettings: Settings,
  ...
): Settings {
  const snapshot: Partial<Record<SettingPath, unknown>> = {};
  for (const key of Object.keys(SETTINGS_SCHEMA) as SettingPath[]) {
    snapshot[key] = baseSettings.get(key);
  }
```

returning `Settings.isolated({...snapshot, ...})` (`executor.ts:793-804`).
The profile still reaches the whole subtree — each level snapshots a parent
that already carries the profile from boot, so propagation is TRANSITIVE
snapshot-at-spawn — but a mid-session settings mutation would NOT
retro-propagate to running subagents. Moot under the teardown+relaunch model,
but a hard constraint: **no executor may assume live mid-session profile
propagation** (§Global Constraints).

### In-process subagent topology: workers are OMP subagents

"Worker" tasks are **in-process OMP subagents inside the Manager's
container**, NOT spawned Compass peer containers. This avoids a container/
microVM per worker and matches what workers are: ephemeral task executors,
not organizational identities.

- **No Compass handle, account, or channel.** A subagent has SDK-native
  lifecycle only: on completion it stays interrogable — "Keep-alive: finished
  and failed subagents both stay interrogable. The lifecycle manager owns
  idle-TTL parking + revival from here on" with
  `registry.setStatus(args.id, "idle")` and
  `AgentLifecycleManager.global().adopt(...)`
  (`@oh-my-pi/pi-coding-agent@16.5.2 src/task/executor.ts:1989-1995`) — and a
  parked subagent is revived for follow-up turns via
  `runSubagentFollowUpTurn`/`FollowUpTurnOptions` ("Registry id of the (live
  or parked) subagent to continue", `executor.ts:1999-2016`). Addressing is
  by registry id over OMP-internal IRC.
- **Survival ruling: ephemeral.** On a Manager relaunch (including a profile
  change), the Manager resumes its session — the prior transcript, including
  COMPLETED subagent results, survives in the resumed history — and
  RE-SPAWNS any subagents it still needs. In-flight subagents are lost on
  relaunch. There is no cross-container subagent persistence and no handle
  to resume against; this is the accepted cost of the no-identity model.
- **Recursion works today — for model/settings/IRC, not UI trace.** Subagent
  model resolution, settings snapshotting, and IRC gating all recurse
  (`isIrcEnabled` returns true for any `taskDepth > 0`,
  `src/tools/irc.ts:44-45`; the default recursion ceiling is
  `task.maxRecursionDepth` = 2, `config/settings-schema.ts:4179-4181`), so a
  Manager's subagents can themselves spawn subagents under the same profile,
  each level snapshotting the profile-bearing settings (§Propagation spine).
  UI visibility does NOT recurse the same way — the session-log trace is
  two-level (§UI seam).

### Tool split: Compass comms for the Manager, OMP IRC for subagents

The split is: **Compass comms tools → Manager ONLY** (all Compass
Manager↔human/peer communication); **subagents → OMP-internal IRC ONLY**
(talk to each other and their Manager; no Compass comms tools).

This split largely holds **by construction** on 16.5.2, which the record
pins rather than re-plumbs:

- The Compass comms/lifecycle/forge tools reach ONLY the Manager's session:
  they are `customTools` on the Manager's `createAgentSession` call
  (`customTools: [...mcp.tools, ...nativeTools]`, `cli.ts:896`). A subagent
  session's `customTools` are rebuilt from the parent's `mcpManager` only —
  `const mcpProxyTools = options.mcpManager ? createMCPProxyTools(options.mcpManager) : []`
  (`executor.ts:2394`) passed as
  `customTools: mcpProxyTools.length > 0 ? mcpProxyTools : undefined`
  (`executor.ts:2489`) — and the Compass session passes tools, not an
  `mcpManager` (`enableMCP: false`, `cli.ts:897`), so no Compass tool ever
  reaches a subagent session.
- Subagents always get IRC: "a restricted whitelist must still carry `irc`
  for the subagent to actually use it" — the executor force-includes it
  (`executor.ts:2194-2198`) and gates activation on `isIrcEnabled`
  (`executor.ts:2218`; depth > 0 is always enabled, `irc.ts:44-45`).
- The subagent toolset is whitelist-filtered: parent-owned tools are
  stripped post-construction (`parentOwnedToolNames` filter +
  `setActiveToolsByName`, `executor.ts:2546-2551`), and a subagent
  definition's `tools:` list is the whitelist seam
  (`executor.ts:2183-2198`) for any further narrowing.

What the record must ADD (tasks T7/T9): a test pinning the by-construction
exclusion (so an SDK upgrade or a future `mcpManager` handoff cannot
silently leak comms tools into subagents), and **system-prompt instruction
updates making the split explicit** — the Manager's prompt says subagent
redirection requests arrive via its channel and it steers workers over
IRC/follow-up turns; the subagent-facing prompt copy says peers and the
Manager are reached via IRC registry ids and there is no Compass
channel/handle to post to.

### UI seam: subagent work nests under the Manager in the session log

The human sees subagent work in the Manager's session log, nested under the
Manager — subagents have NO channel; the human pings the MANAGER in the
comms channel to redirect a subagent.

The SDK surfaces the DIRECT-CHILD layer: the executor subscribes to each
child session it spawns and re-emits every child event on the SPAWNING
session's bus — `args.eventBus.emit(TASK_SUBAGENT_EVENT_CHANNEL, { id,
event })` (`executor.ts:1193-1199`, wired per child session at
`executor.ts:1472-1474`; the bus is `this.session.eventBus`,
`task/index.ts:1442`) — and in-flight subtree snapshots ride the child's own
`tool_execution_update` ("we stash the latest snapshot so the parent UI can
render the in-flight subtree", `executor.ts:1322-1335`).

**Two-level trace is the explicit v1 contract.** The re-emission does NOT
recurse: a child session's own spawns land on the CHILD's private EventBus —
the child session options pass no `eventBus` key (`executor.ts:2437-2496`)
and `createAgentSession` defaults a fresh bus
(`const eventBus = options.eventBus ?? new EventBus()`, `sdk.ts:1120`) — so
nothing at Manager level subscribes to a grandchild's stream; grandchild
activity is visible only inside the child's `tool_execution_update`
snapshots. v1 therefore renders the Manager plus its DIRECT children. An
SDK-fork recursive-re-emit path is OUT of v1 scope (the default recursion
ceiling is depth 2 anyway, `task.maxRecursionDepth`,
`config/settings-schema.ts:4179-4181`). T8 pins the boundary: a fixture
asserts grandchild events do NOT surface, so a future SDK change that starts
forwarding them is a visible behavior change, not silent drift.

The Compass frame-producer contract is
`packages/compass-agent/src/mapping.ts` (AgentSessionEvent → compass.v1
`AgentFrame`; the agent's own testable surface, no Runner-side translator)
plus `transport/frame-sink.ts`. Today `EventMapper` maps only the Manager's
own stream, stamping a monotonic `event_id` per event and `message_id` per
assistant message through the single `#sessionEvent` construction point
(`mapping.ts:87-96`, `:239-247`). The reframe adds (task T8, driver-owned
surface):

- `SessionEvent` gains a `string subagent_id = 10` discriminator (additive
  append to `compass.proto:447-461`; empty = the Manager's own event, so
  every existing frame is unchanged).
- The agent drains `TASK_SUBAGENT_EVENT_CHANNEL` `{ id, event }` pairs and
  routes each child event through the mapper with `subagent_id = id`,
  reusing the same event_id/clock stamping so the nested trace is ordered on
  the one monotonic sequence.
- The renderer folds by `subagent_id` to nest worker trace under the
  Manager. No account, no channel, no comms frames — session frames only.

### Adjacency: compose, do not duplicate

- **RIG-1715 / PR #671 — the LLM gateway (merged).** The frozen record at
  `docs/designs/server/compass-server-llm-gateway/design.md` is truth (the
  PR body is stale): the OMP fork's auth-gateway runs directly as a
  standalone supervised process, Compass builds adapters only, Go re-port
  demoted to RIG-2843 (`design.md:38-47`). It is the single LLM egress —
  agents hold no provider creds; T5 materializes
  `COMPASS_LLM_GATEWAY_URL`/`_TOKEN` into the container
  (`design.md:860-869`). **The profile's `models` axis resolves THROUGH this
  gateway**: a profile names models; the gateway owns credentials and
  backend egress. Nothing in this record touches egress.
- **RIG-2845 — Compass stable-name provider routing (in design, PR
  #725).** The routing policy above the gateway substrate: stable Compass
  model names (e.g. `claude-opus-4-8`) and the stable-name→backend-order
  fallback chain, consuming RIG-1715 pool resolution and RIG-2562 evals (the
  gateway record forward-refs it: "a policy layer ABOVE this credential
  substrate — designed separately in RIG-2845",
  `compass-server-llm-gateway/design.md:407-413`). **Boundary (updated —
  RIG-2845 narrowed to routing-only, 2026-08-30): this record owns model
  SELECTION — a profile's `models.manager`/`models.agents` fields ARE which
  model each agent uses — plus its per-Manager propagation; RIG-2845 owns
  only the stable-name VOCABULARY + upstream ROUTING those model fields
  reference.** The originally-scoped Compass role-taxonomy half of RIG-2845
  was dropped in the narrowing: model selection is the profile's (this
  record) and the fleet-global tier defaults are OMP's built-in `modelRoles`
  — RIG-2845 defines no role→model policy layer. A profile's model-field
  VALUES are drawn from RIG-2845's stable-name vocabulary once it lands;
  this record does not redesign that vocabulary or its routing.
- **RIG-1716 — embedded MCP gateway (Backlog).** One Server endpoint holding
  all tool auth. The profile's `extensions` axis (schema'd, consumption
  deferred) will reference a tool-set/ACL against THAT gateway, not
  per-container MCP creds. Composition noted; the axis is deferred, so there
  is no hard dependency.

### The two-layer contract, reframed at profile granularity

The authoring-vs-selection framing survives the reframe intact — it now
operates on profiles:

- **Layer 1 — bounded SELECTION.** A profile NAME is a bounded selection: it
  names a VC'd bundle in the server-provisioned registry. Any agent-facing
  surface that carries a profile (the spawn path — ruled YES unconditional,
  see Resolved decisions) can only POINT AT pre-provisioned config, never
  inject free text. The
  anti-injection invariant is preserved by the shape itself: like `role`
  (which selects a server-provisioned `prompts/<role>/SYSTEM.md`,
  `compass.proto:589-600`) and `model` (which selects a `models.yml` entry),
  `profile` selects a registry entry. The agent SELECTS; it never AUTHORS.
- **Layer 2 — operator AUTHORING.** Profile CONTENTS are operator-authored
  in the VC'd registry, reviewed as code, and delivered through the
  server-authoritative store-door → mount pipeline. Layer 2 is never
  agent-facing: no agent-facing surface accepts profile bytes, only a
  profile name. Free-text authoring (a caller-supplied block-0, an inline
  settings blob) stays walled off exactly as today's provision path walls
  off persona/role injection (`service.go:159-165`).

### Cross-family review constraint, at profile granularity

`Reviewer.family ≠ Implementor.family` and `Design-critic.family ≠
Designer.family` remain wave/tree-composition config, not a wire field. The
reframe moves the check to profile granularity: family is a registry
function of a profile's model selections (each `models.*` value resolves to
a `models.yml` entry whose family is explicit reviewable data, §Shared
anchors), so any holder of the profile registry can check the constraint
statically — a well-formed profile pairs a reviewer model whose family
differs from the implementor model's WITHIN the profile, and the registry
review (a PR gate on `profiles/`) is where the check runs. The constraint is
ADVISORY: it is expressed and checked at `profiles/` PR REVIEW time as a
reviewer-facing check, not a hard store-door rejection (reverted from the
interim machine-lint posture by Matt's OQ-1 ruling — "cross-family stays
advisory"). On the harness lane the equivalent check is machine-enforced
(rollout-artifact lint); Compass deliberately stays advisory here. This sits
adjacent to harness OQ6 (within-profile vs across-wave family binding):
Compass expresses the constraint within-profile; if the harness resolves OQ6
toward across-wave MACHINE binding, Compass revisits — cross-referenced, not
decided here.

## Alternatives considered

- **The per-spawn `{model, role}` tuple (the superseded prior draft of this
  record).** A parent names the child's model/role per `SpawnPeerRequest`,
  threaded store → provision → runner env per child container. Superseded
  (Matt, 2026-08-27): the unit of configuration is wrong twice over — config
  scatters across per-spawn call sites instead of living in one named,
  reviewable, VC'd bundle, and it presumes workers are peer CONTAINERS.
  Under the in-process topology there is no per-worker container to env-pin;
  the profile covers the subtree through the settings spine instead. The
  grounded store→provision→runner pipeline the draft designed survives,
  re-pointed at one `profile` name instead of a model selector (§Plan T1-T4).
- **Workers as Compass peer containers.** Each worker a spawned peer with
  handle/account/channel. Rejected: a container/microVM per ephemeral task
  is the wrong cost shape; identity outlives the work (orphaned accounts and
  channels per task); and the SDK already owns exactly this lifecycle
  in-process (idle/park/revive, `executor.ts:1989-1995`). Peer spawn remains
  the right shape for durable organizational identities — sub-managers with
  channels — just not for task workers.
- **Keep the model Runner-global and roll out per-Runner.** Rejected: one
  Runner hosts many Managers, so per-Runner granularity cannot express "this
  Manager's tree on candidate, that one on known-good" — the exact eval
  rollout shape RIG-2936 needs.
- **Adopt OMP's model-stack profile schema verbatim.** Rejected (Matt's
  explicit call): Compass owns far more of agent/config delivery — prompt
  corpus, extensions/MCPs, session settings — so its profile is a superset
  designed from scratch. The shared anchors are the concept, registry
  discipline, selector grammar, family-as-data, and the `manager` key — not
  the schema.
- **Live profile mutation (re-render settings into a running session).**
  Rejected as the change mechanism: on 16.5.2 subagent settings are
  snapshot-at-spawn (`executor.ts:768-776`), so a live re-render would apply
  to the Manager but silently NOT to running subagents — a split-brain tree.
  Teardown + re-provision + relaunch with resumed session applies the
  profile uniformly at boot by rebuilding the handle from the store row (T4);
  it composes existing primitives (despawn teardown, provision, resume) but
  is NOT a single plain `Reload` — a `Reload` reuses the provision-captured
  handle and would relaunch stale (`host.go:857,878`).
- **A per-spawn profile parameter on the task tool (per-worker profiles).**
  Rejected for v1: the profile is per-MANAGER by ruling; per-worker variation
  already exists WITHIN the profile via `models.agents` per-role keys, which
  is the granularity eval output actually produces.

## Global Constraints

- **No executor may assume live mid-session profile propagation.** Harness
  17.3.1 shares parent Settings by reference (live, arbitrary depth);
  Compass's SDK — lockfile-pinned at 16.5.2 (manifest range `^16.4.8`, so
  `<17.0.0` and the 17.x live-reference line is excluded;
  `package.json:21`, `bun.lock:694`) — `createSubagentSettings` snapshots
  per key (`executor.ts:768-776`, `Settings.isolated({...snapshot})`
  `executor.ts:793-804`). The profile reaches the subtree by TRANSITIVE
  snapshot-at-spawn only. Profile application is boot-time (teardown +
  re-provision + relaunch); the snapshot-semantics assumption is
  lockfile-guarded, not manifest-pinned, and additionally pinned by T7/T8
  behavior tests, so any future SDK upgrade to live-reference semantics is a
  behavior change to re-review, not a compatibility assumption.
- **Overlay lifetime.** Any rendered profile artifact consumed at spawn time
  must outlive the session: rendered to a durable per-codename path, never a
  self-reaping tmpfile (harness constraint, adopted — their --config overlay
  is re-read from disk on EVERY spawn). On Compass the registry is the
  durable source and the read-only container mount is the durable
  materialization; the entrypoint's rendered settings overlay lives for the
  container's lifetime.
- **Additive wire contract.** No wire-breaking change to `SpawnPeerRequest`
  existing fields (`handle = 1`, `display_name = 2`, `client_request_id = 4`);
  field 3 is `reserved`/`initial_prompt` (DL-187) and is NOT reclaimed
  (`agent_gateway.proto:170-171`); RIG-2673 claims `role = 5`/`persona = 6`;
  new fields take fresh numbers. The `SessionEvent` `subagent_id`
  discriminator is a non-breaking append to the oneof-bearing message
  (`compass.proto:446-461`: "Additions to the `event` oneof are non-breaking
  appends"; a new scalar field is likewise additive).
- **Owner inheritance invariant preserved.** Spawn creates peers under the
  CALLER'S OWNER, never a request-carried owner; nothing in this record adds
  an owner-shaped field. Subagents have no owner at all — no account exists.
- **Anti-injection posture — and "no injection" is not "no harm".** Every
  agent-facing config surface stays a bounded selection: `profile` names a
  registry entry, `role` names a mount prompt, `model` selectors name
  `models.yml` entries. Free text never rides an agent-facing field; profile
  CONTENTS are operator-authored in VC and delivered via the
  server-authoritative store-door → mount pipeline (Layer 2). What bounded
  selection does NOT bound is the SELECTABLE SET: the registry is
  fleet-global, so a selector can name any vetted profile, including one
  vetted for a different context. That composition surface is the accepted
  residual oversight-degradation risk (Resolved: OQ-1 ruled YES,
  unconditional), now at profile granularity.
- **The tool split is a capability contract, NOT a containment boundary.**
  The comms/IRC split (§Tool split) shapes what a WELL-BEHAVED agent can
  reach; it is not a security boundary, and no authz story may ever be built
  on it. Subagents run at full Manager trust: same container, UID,
  filesystem, agent socket, and gateway credentials; headless children are
  forced to yolo approval (`createSubagentSettings` sets
  `"tools.approvalMode": "yolo"`, `executor.ts:793-804`); and cwd
  `.omp/tools/` custom tools load into subagent sessions unconditionally
  (`discoverCustomToolPaths([], cwd)` runs regardless of the mount contract,
  noted at `cli.ts:885-888`). T7's closed-set pin is a drift alarm, not a
  fence.
- **Set-at-creation-only.** The stored `profile` follows RIG-2673's
  semantics rider: an idempotent re-spawn or resume runs under the STORED
  value, never a retry request's (`lifecycle.go:317-331` threads
  `existing.Agent.*` on both non-create arms). A profile CHANGE is an
  explicit teardown + relaunch, not a mutation through any agent-facing
  path.
- **Server-authoritative provision threading.** Every new
  `ProvisionAgentWorkspaceRequest` field carries the same
  SERVER-AUTHORITATIVE overwrite-from-store contract and test shape as
  persona/role (`service.go:159-165`;
  `service_placement_pgtest_test.go` overwrite pairs).
- **Selector grammar is opaque outside the shared split.** Selectors split
  on the last colon for the thinking suffix (a valid effort-ladder level in
  `models.yml`) and are otherwise opaque — never parsed by any Compass
  component (`cli.ts:138-140`). Family is registry data, not name-parsing.
- **Traversal-guarded labels.** Every by-reference label used as a path
  segment (`role`, now `profile`) passes the `readMountedRolePrompt` guard
  shape (reject `/`, `\`, `..`; `config-reader.ts:374-381`); a rejected or
  malformed `profile` label resolves to `default`, never partial injection —
  and a MISSING `default` profile fails loud at boot; there is no no-profile
  path.
- **Migration convention.** New store columns go into the collapsed
  `0001_init.sql` (seed-forward posture, `0001_init.sql:10-20`; contiguity
  guard `store.go:287-294` refuses gapped versions), NOT a new numbered
  migration. The `profile` column is `NOT NULL DEFAULT 'default'` — NOT the
  `DEFAULT ''` of `persona`/`role` (`0001_init.sql:78`/`:80`) — because
  profile is mandatory: every account names one, `default` at minimum.
- **Proto discipline.** Regen via the repo's buf lanes; generated code is
  never hand-edited.
- **Naming.** The managed offering is described as "private,
  commercially-licensed"; the private monorepo is never named.

## Plan

Dependency order: T1 (registry + store door) and T2 (proto + regen) are
independent roots; T3/T4 (server) build on T2; T5 (runner) builds on T2 and
T1's mount layout; T6/T7/T8/T9 (agent) build on T1/T2 and land in that
order; T10 (harness) is alignment-only. Each task is the smallest unit
carrying its own test cycle and becomes a filed impl issue after freeze.
**Sibling-record serialization:** RIG-2673 lands `role = 5`/`persona = 6`
and rewrites the same `SpawnAsAccount` literal, `provisionAndStart`
signature, and pgtest overwrite pairs T2/T4 touch — T2+T4 here serialize
AFTER (or merge with) RIG-2673's proto/server tasks; if RIG-2673 has not
landed first, T2 carries all three `SpawnPeerRequest` fields.

### T1 — Profile registry: schema, store-door top-dir, materialization

Owner: compass-server

Define the profile superset schema (§Approach) and admit `profiles/` as a
new whitelisted top-dir at EVERY layer that enumerates the bundle's top
dirs — there are THREE independent whitelists, all currently listing the
same six dirs, and a bundle admitted at only one layer fails the whole
fleet unpack:

- Store door: `agent_config.go` gains `topDirProfiles = "profiles"` beside
  the existing six (`agent_config.go:35-42`) and the `configBundleTopDirs`
  set (`agent_config.go:57-64`); the rejection message names the set
  (`agent_config.go:492-493`). The per-member grammar (`configMemberParts`
  plus `validateRegularMember`) gains an arm admitting exactly
  `profiles/<name>/profile.yml` (`<name>` follows the existing
  `configNamePattern` segment grammar).
- Runner unpack: `config_materialize.go` gains the same constant in
  `configTopDirs` (`config_materialize.go:71-78`; rejection at `:483-484`)
  plus a `validateNestedMember` arm for the `profiles/<name>/profile.yml`
  grammar (`config_materialize.go:511-551`).
- CLI bundle builder: `bundle.go`'s mirrored whitelist ("Kept in sync with
  the door constants", `bundle.go:23-28`; `bundleTopDirs` `bundle.go:63-70`;
  `errNotWhitelisted` `bundle.go:177-179`) gains the dir and the member
  grammar.

The `ConfigMaterializer`'s generic unpack machinery is otherwise reused, but
"the materializer needs no change" is FALSE — its whitelist and member
grammar are two of the three layers above. Cautionary precedent: `prompts/`
(the role block-0 exemplar, `config-reader.ts:370-390`) is in NONE of the
three whitelists today, so the bundle pipeline cannot deliver it (§Grounding
delivery-gap caution) — do not replicate that gap for `profiles/`.

Ship a `default` profile whose contents mirror the committed fleet config
(the committed config IS `default`). Schema validation runs at the store
door, failing closed before any byte lands, matching the bundle's existing
untrusted-tarball posture:

- yml parse + `models.*` selector shape.
- Superset-key closure: the door ACCEPTS every superset key
  (`models`/`corpus`/`extensions`/`settings` — deferred axes included) and
  REJECTS keys OUTSIDE the superset. "Unknown-key rejection" means
  outside-the-superset rejection, never deferred-axis rejection.
- `models.agents` key lint: every key must match the FRONTMATTER `name:` of
  a shipped `agents/*.md` def in the SAME bundle — NOT its filename stem.
  The SDK resolves subagents by `agent.name` (`getAgent`
  `task/discovery.ts:143-144`; `parseAgentFields` requires a frontmatter
  `name`, `helpers.ts:242`) and consults the override record per spawned
  `agentName` (`task/index.ts:1271-1281`), so a key matching no def name is a
  SILENT no-op at spawn time; the lint parses each def's frontmatter `name`
  and checks keys against that set. (The fleet ships a def whose frontmatter
  is `name: implementer` in `config/agents/implementer.md` — stem and name
  coincide there; the lint must still key on the frontmatter name so a def
  whose name diverges from its stem lints correctly. A profile keying
  `implement` would otherwise roll out as a fleet-wide silent default.)
- Cross-family posture (§Cross-family review constraint): ADVISORY at
  `profiles/` PR review — the reviewer-vs-implementor family pairing is
  checked by reviewers against `models.yml` family data; NOT a door-failing
  lint (per OQ-1's ruling, cross-family stays advisory).

Interfaces:

- Consumes: existing bundle admission (`agent_config.go` member parsing),
  runner unpack validation (`config_materialize.go`), and the CLI bundle
  builder (`bundle.go`).
- Produces: `profiles/<name>/profile.yml` admitted at all three layers; the
  documented profile superset schema; the `default` profile convention; the
  superset-key and `models.agents`-key lints (cross-family stays an
  advisory PR-review check, not a door lint).
- Test: store pgtest — a bundle with `profiles/candidate/profile.yml` is
  admitted and materializes under the mount; a malformed profile.yml, a
  stray `profiles/<name>/other.file`, a key outside the superset, or a
  `models.agents` key matching no shipped def's frontmatter `name` is
  rejected at the door; a `models.agents` key matching a def whose
  frontmatter `name` DIVERGES from its filename stem is ACCEPTED (pinning
  that the lint keys on frontmatter name, not stem), while a key matching
  only the stem of such a def is REJECTED; a runner unpack test admits the
  same member (all three whitelists exercised); `default` present in the
  shipped fleet bundle.

### T2 — Proto: `profile` on spawn + provision, regen

Owner: compass-server

`SpawnPeerRequest` gains `string profile = 7` (after RIG-2673's
`role = 5`/`persona = 6`; field 3 stays reserved, DL-187) — optional; empty
= the `default` profile. `ProvisionAgentWorkspaceRequest` gains
`string profile = 5` with the SERVER-AUTHORITATIVE doc contract mirroring
persona/role (`compass.proto:579-600`). The operator profile-change RPC is
specified here so T4 is implementable as scoped — CompassService gains:

```proto
rpc SetAgentProfile(SetAgentProfileRequest) returns (SetAgentProfileResponse)

message SetAgentProfileRequest {
  string agent_id = 1; // the agent account to re-pin
  string profile = 2;  // registry profile name; empty = default
}

message SetAgentProfileResponse {}
```

admin-gated on the network door (the AdminGate classifies it adminOnly,
mirroring the existing admin-gated operator write posture — `PutAgentConfig`
is the exemplar, `agent_config_service.go:4-5`). The handler updates
`agent_accounts.profile` and triggers T4's teardown + relaunch with resumed
session. The AGENT-triggered variant is DEFERRED — v1 is
set-at-creation-only (§Global Constraints); agent-triggered live mutation is
out of v1 scope (see Resolved decisions). Regen all three buf lanes.

Interfaces:

- Consumes: existing message shapes (`agent_gateway.proto:167-173`,
  `compass.proto:563-601`); the admin-gate classification table.
- Produces: `SpawnPeerRequest.GetProfile()`,
  `ProvisionAgentWorkspaceRequest.GetProfile()`, the `SetAgentProfile`
  service surface, TS mirrors in
  `packages/compass-agent/src/gen/compass/v1/`.
- Test: generated code compiles in both languages; buf lint passes
  (additive).

### T3 — Store: `profile` column + creation threading

Owner: compass-server

`agent_accounts` gains `profile TEXT NOT NULL DEFAULT 'default'`, added
directly to the collapsed `0001_init.sql` `CREATE TABLE agent_accounts` beside
`persona`/`role` (`0001_init.sql:74-80`) — NOT a new numbered migration. The
default diverges DELIBERATELY from the `DEFAULT ''` of `persona`/`role`:
profile is MANDATORY (no non-profile agent), so every account names a profile,
`default` at minimum, and the empty-profile state does not exist
(seed-forward posture `0001_init.sql:10-20`; contiguity guard
`store.go:287-294`). `store.NewAgent` (`inputs.go:20-37`) gains
`Profile string`; the `CreateAgent` INSERT and `AgentAccount` read model
thread it verbatim (the store stores, never synthesizes).

Interfaces:

- Consumes: existing `CreateAgent(ctx, ownerUserID, NewAgent)`.
- Produces: `store.NewAgent{Profile string}`; `AgentAccount.Agent.Profile`.
- Test: pgtest round-trip — create with `profile` set, read back verbatim;
  `default` for a bare create (verifying the `NOT NULL DEFAULT 'default'`
  column default — the empty-profile state does not exist).

### T4 — Server: spawn threading + provision overwrite

Owner: compass-server

In `SpawnAsAccount` (`lifecycle.go:161-203`): populate
`Profile: req.GetProfile()` in the `store.CreateAgent` literal (the spawn
path carries `profile` unconditionally — OQ-1 ruled YES; the server accepts
it from the agent-facing caller, the owner-fence bounding the child account
as today).
Thread `created.Agent.Profile` through `provisionAndStart` onto
`ProvisionAgentWorkspaceRequest`; both non-create arms
(`lifecycle.go:317-331`) thread `existing.Agent.*` — extend the field set.
To avoid a three-plus-positional-same-typed-string signature, pass a
`store.AgentOverrides{Persona, Role, Profile}` struct. In the operator
provision handler (`service.go:159-165`): extend the overwrite-from-store
block with `req.Msg.Profile = acc.Agent.Profile` (cleared for non-agent
accounts). Implement the operator profile-change path behind T2's
`SetAgentProfile` RPC (admin-gated) as the explicit orchestration: the
handler updates `agent_accounts.profile`, then tears the container DOWN
(the despawn/Remove teardown path, identity-preserving) and
RE-PROVISIONS it so the Runner rebuilds a fresh `AgentHandle` from the
updated store row (`provisionAndStart` reads the account's stored
`Profile`), then Starts with resumed session context
(`COMPASS_RESUME_SESSION_FILE`; the resume-file threading exists,
`agent_exec.go:92-94`). A plain `Reload`/`RefreshConfig` is INSUFFICIENT:
`reloadLocked` re-resolves the handle from the registry
(`host.go:857`) and derives env via `agentEnv` from the provision-captured
handle spec (`host.go:878`, `AgentHandle.spec` `agent.go:62-90`), and there
is no per-container Deprovision RPC — a Stop/Reload reuses the container and
its captured spec (`host.go:232`) — so a Reload would relaunch with the
STALE profile and the store write would no-op. Re-provision, not Reload, is
the only path that re-threads a changed profile.

Interfaces:

- Consumes: T2 getters, T3 store fields.
- Produces: extended `provisionAndStart(ctx, agentID string, ov
  store.AgentOverrides, req)`; the profile-change relaunch path.
- Test: pgtest — spawn with `profile` set → row value AND Provision wire
  carries it; operator-provision overwrite pair (client sends bogus value,
  Runner receives store value); a `default`-profile account → Provision
  carries `default`; idempotent re-spawn keeps stored values; a `SetAgentProfile` change
  re-provisions so the relaunched container's `COMPASS_PROFILE` env carries
  the NEW profile (asserting the change took effect, not merely that the
  store row updated) under resumed session context.

### T5 — Runner: `COMPASS_PROFILE` env threading

Owner: compass-runner

`runtime.AgentSpec` gains `Profile string` with an `AgentHandle.Profile()`
accessor mirroring `Persona()`/`Role()`; `SpecBuilder.BuildSpec` copies it
from `ProvisionAgentWorkspaceRequest`. `agentEnv` (`host.go:878-887`) adds
`Profile: handle.Profile()`; `execSpec` injects `COMPASS_PROFILE` beside the
existing keys (`agent_exec.go:83-91`) — always populated, since every account
carries a profile (store default `'default'`), so the omitted-when-empty arm
is defensive, never the steady state. The Runner-global
`AgentModel`/`COMPASS_MODEL` is retained UNCHANGED as pre-existing infra, but
is no longer a model fallback for a Compass-provisioned session: every such
session is profile-bearing, so the profile model-stack always supersedes it.
**Spec-survival case:** a handle reconstructed on the
reattach/recovery path (`h.registry.Resolve(name)`) must still carry
`Profile`, else a pinned Manager silently reverts to `default` on Runner
reload — the implementor MUST confirm the handle carries `Profile` after a
process-restart reattach; if registry rehydration does not re-thread store
values, that is a BLOCKER for T5, not a test note.

Interfaces:

- Consumes: T2 provision field.
- Produces: `AgentHandle.Profile()`; env contract `COMPASS_PROFILE` (name
  only; contents come from the mount).
- Test: agentenv/exec tests — profile present ⇒ env carries it; the defensive
  empty arm omits the key (never the steady state — every account carries a
  profile); reattach-path spec survival.

### T6 — Agent: profile resolution + model-stack render

Owner: compass-agent

The entrypoint reads `COMPASS_PROFILE` (always set — every account carries a
profile, store default `'default'`), resolves `profiles/<name>/profile.yml`
from the mount via a new traversal-guarded `readMountedProfile` (guard shape
`config-reader.ts:374-381`; a malformed or traversal-rejected label →
`default`; **an absent `default` profile is a HARD boot failure** — fail-loud,
never a silent no-profile path; `default` is shipped by T1 and MUST resolve),
and renders the v1 model-stack: `models.manager` → the session `modelPattern`
(the profile is the sole model source for a Compass-provisioned session,
superseding the Runner-global `COMPASS_MODEL`); `models.agents` →
`task.agentModelOverrides` merged onto the settings overlay
`buildFleetSettings` builds (`cli.ts:497`, `:782-785`). Deferred
axes (`corpus`/`extensions`/`settings`) are parsed and IGNORED with a
logged notice, so a future profile using them degrades loudly, not
silently.

The ruling (was OQ-2 — see Resolved decisions): a profile-named model that
fails auth/registry resolution FAILS LOUD on ALL sessions — a frame-visible
error and a failed spawn, never a silent substitution. The SDK's
`resolveModelOverrideWithAuthFallback` would substitute the parent's active
model with only an `authFallbackUsed` flag and a warning string
(`model-resolver.ts:1261-1297`, invoked at `executor.ts:2329-2337`); the
fallback branch is gated on `parentActiveModelPattern`
(`model-resolver.ts:1275`), but the SDK's subagent runner populates
`modelPatternAuthFallback` from the parent whenever a per-agent override is
present with no concrete model (`executor.ts:2443-2445`) — so on pinned
16.5.2 suppressing the silent substitution across the subagent subtree is
NOT achievable purely from Compass's render. Known implementation coupling
for T6 (confirm at implementation): Compass must ensure the profile model
resolves-or-fails-loud, and if the SDK's baked-in subagent fallback cannot
be disabled through supported settings on 16.5.2, T6 carries a pinned SDK
patch / upstream ask removing the silent substitution for
Compass-provisioned sessions. The frame-visible telemetry stays — as the
surface of a HARD failure, not a degraded-but-proceeding fallback.

Delivery is per-quadrant (session × failure kind). For the MANAGER session,
fail-loud on registry NON-resolution is already delivered by the boot
model-health belt: once `models.manager` is rendered as the pinned
`modelPattern`, the belt throws (refusing to boot model-less) exactly when
`modelPattern !== undefined && session.model === undefined`
(`cli.ts:991-1005`) — i.e. the model does not resolve against the registry,
the common eval-rollout case (a profile naming a not-yet-registered model).
A `models.manager` value that RESOLVES in the registry but has no working
auth leaves `session.model` SET (the Manager session sets no
`modelPatternAuthFallback`, so the `sdk.ts:2011-2098` deferred-resolution
loop does not auth-gate it), the belt passes, and the auth failure surfaces
as a frame-visible HARD error at first turn, not at spawn. So "the spawn
fails" is exact for the registry-non-resolution quadrant; the
auth-fail-on-an-otherwise-resolvable-model quadrant fails loud at first turn
unless T6 extends the belt to pre-flight the pinned model's auth. T6 MAY
extend the belt to auth-pre-flight the pinned Manager model (making that
quadrant also fail at spawn); either way the failure is frame-visible and
hard, never a silent substitution.

Interfaces:

- Consumes: T1 mount layout; T5 env var; existing `buildFleetSettings` +
  `createAgentSession` seams.
- Produces: `readMountedProfile`; profile-rendered
  `modelPattern` + `task.agentModelOverrides`.
- Test: unit — profile with `manager` + `agents` renders both surfaces;
  absent `COMPASS_PROFILE` resolves to `default`; a MISSING `default` profile
  is a hard boot failure (fail-loud, no `COMPASS_MODEL` fall-through);
  traversal labels rejected; deferred-axis content produces the notice and no
  behavior change; e2e via the createSession seam asserting the settings
  overlay carries the overrides; a profile naming a model that does NOT
  resolve against the registry produces a frame-visible HARD failure with
  the spawn failing (the belt at `cli.ts:991-1005` fires); a profile naming
  a model that resolves but has no working auth produces a frame-visible
  HARD error (at spawn if T6 extends the belt to auth-pre-flight, else at
  first turn) — never a silent substitution to the parent model in either
  case.

### T7 — Agent: subagent tool-split whitelist + pin test

Owner: compass-agent

Pin the comms/IRC split with a CLOSED-SET assertion. The exclusion is
by-construction today (Compass tools are Manager-session `customTools`,
`cli.ts:896`; subagent sessions rebuild `customTools` from `mcpManager`
only, `executor.ts:2394`/`:2489`, and Compass passes tools, not a manager) —
add a test that constructs a subagent session shape and asserts its toolset
equals EXACTLY the expected tool names (a closed set, not a deny-list): this
reddens both a leaked Compass comms/lifecycle/forge tool AND a
cwd-discovered stray (`.omp/tools/` custom tools load unconditionally,
`cli.ts:885-888`), so an SDK upgrade or a future `mcpManager` handoff cannot
silently widen the set. Express any further narrowing through the
subagent-definition `tools:` whitelist seam (`executor.ts:2183-2198`; `irc`
force-included `:2196-2197`).

Interfaces:

- Consumes: the SDK subagent construction seams (16.5.2 pins above).
- Produces: the pinning test; documented whitelist convention for Compass
  subagent definitions.
- Test: the closed-set pin itself (red if the subagent toolset differs from
  the expected exact name set — a Compass tool leaking in, a stray cwd tool,
  or `irc` missing).

### T8 — Agent: mapping.ts subagent-nested session frames

Owner: compass-agent (the driver's own frame-producer contract)

`SessionEvent` gains `string subagent_id = 10` (additive,
`compass.proto:447-461`; empty = Manager's own event). The agent subscribes
to `TASK_SUBAGENT_EVENT_CHANNEL` (`executor.ts:1193-1198` emit shape
`{ id, event }`) and routes each child `AgentSessionEvent` through
`EventMapper` with `subagent_id = id`, reusing the single `#sessionEvent`
stamping point (`mapping.ts:239-247`) so event_id/at_unix_ms stay one
monotonic sequence; per-subagent `message_id` correlation must be scoped
per child stream (the current `#messageSeq` is mapper-global,
`mapping.ts:88-96` — either one mapper instance per child or a
subagent-scoped counter; implementor's choice, test-pinned). Lifecycle
`state` frames remain Manager-only (a subagent's park/revive is trace, not
board state). The renderer folds by `subagent_id` to nest worker trace
under the Manager.

Scope pin: this surfaces the TWO-LEVEL trace only (§UI seam) —
`TASK_SUBAGENT_EVENT_CHANNEL` carries direct children; grandchild events
stay on the child's private bus (`sdk.ts:1120`). A fixture PINS that
grandchild events do NOT surface (no frame attributes a grandchild
`subagent_id`), so an SDK change that starts forwarding them is a visible
behavior change.

Interfaces:

- Consumes: T2 regen (SessionEvent field); the SDK subagent event channel.
- Produces: nested-session-frame contract (`subagent_id` on every
  child-originated SessionEvent); mapper wiring.
- Test: mapping fixtures — a child tool_execution/text event maps to a
  frame with `subagent_id` set and correct per-child message coalescing;
  Manager events keep `subagent_id` empty; unmapped child variants surface
  as counted UnmappedEvents (existing invariant); the grandchild
  non-surfacing pin (above).

### T9 — Agent: system-prompt instruction update for the comms/IRC split

Owner: compass-agent

Make the tool split legible to the agents themselves. Manager-facing prompt
copy (the `prompts/<role>/SYSTEM.md` corpus for manager roles): subagents
are in-process workers with no Compass handle/channel; the human redirects
a worker by pinging the MANAGER on its channel; the Manager steers workers
via IRC/follow-up turns; completed worker results survive relaunch in the
resumed transcript, in-flight work does not. Subagent-facing copy (the
task/agent-definition prompts): peers and the Manager are reached via
OMP-internal IRC registry ids ONLY; there is no Compass channel, account,
or comms tool at worker depth.

Interfaces:

- Consumes: T7's split (the behavior the copy describes).
- Produces: updated prompt corpus text (manager + subagent surfaces).
- Test: prompt-copy review against the shipped tool surfaces (advisory
  copy; the enforcement test is T7's pin).

### T10 — Harness alignment (RIG-2935)

Owner: harness

Pointer, not new design. Shared anchors held byte-aligned: profile concept +
VC'd registry discipline; selector grammar (split-last-colon, thinking
suffix a valid effort-ladder level); family as explicit reviewable registry
data; the semantic `manager` key. Schemas explicitly diverge (OMP
model-stack-scoped; Compass superset). Eval (RIG-2562) emits a profile NAME
per run, consumable by both lanes.

Interfaces:

- Consumes: the shared anchors (§Approach).
- Produces: RIG-2935's record as the anchors' home; the eval→profile-name
  handoff both lanes consume.

## Tasks

- [ ] T1 `[compass-server]` profile registry: superset schema, `profiles/`
  admitted at all THREE whitelists (store door, runner unpack, CLI builder)
  with member grammar arms, superset-key + `models.agents`-key lints
  (cross-family advisory at PR review), `default` profile, materialization
  test
- [ ] T2 `[compass-server]` proto: `SpawnPeerRequest.profile = 7`,
  `ProvisionAgentWorkspaceRequest.profile = 5`, `SetAgentProfile` operator
  RPC (admin-gated), regen (3 buf lanes)
- [ ] T3 `[compass-server]` store: `agent_accounts.profile` column +
  `NewAgent.Profile` + round-trip pgtest
- [ ] T4 `[compass-server]` spawn threading + provision overwrite-from-store
  (`AgentOverrides` struct) + profile-change relaunch path + pgtests
- [ ] T5 `[compass-runner]` `AgentSpec`/`AgentEnv` profile field,
  `COMPASS_PROFILE` injection + reattach spec-survival test
- [ ] T6 `[compass-agent]` `readMountedProfile` + v1 model-stack render
  (`modelPattern` + `task.agentModelOverrides`) + fail-loud
  unresolvable-model hard failure (frame-visible; no silent substitution) +
  deferred-axis tests
- [ ] T7 `[compass-agent]` subagent tool-split closed-set pin test (exact
  toolset equality)
- [ ] T8 `[compass-agent]` mapping.ts subagent-nested session frames
  (`SessionEvent.subagent_id`) + fixtures + grandchild non-surfacing pin
- [ ] T9 `[compass-agent]` system-prompt instruction update for the
  comms/IRC split (manager + subagent copy)
- [ ] T10 `[harness]` RIG-2935 shared-anchor alignment pointer

## Open Questions

None load-bearing — both former load-bearing OQs (OQ-1 / RIG-2937 and OQ-2)
are ruled and live in §Resolved decisions below.

## Resolved decisions

- **Subtree composition authority (was OQ-1 / RIG-2937 — RULED YES,
  unconditional; Matt, 2026-08-27).** The agent-facing spawn path
  (`SpawnPeerRequest`, and the task/spawn surface built on it) MAY carry
  `profile`: a Manager may select its subtree's profile. UNCONDITIONAL — no
  selectable-set policy rider is introduced, and no cross-family
  conditioning applies (cross-family stays advisory, below). The
  owner-fence still bounds the child ACCOUNT, unchanged; the
  oversight-degradation-by-composition attack (a supervisor selecting a
  weak or ill-suited profile for the very subtree that reviews its output)
  is a KNOWN, ACCEPTED residual risk at this stage — Matt explicitly
  accepted that this "leaves the composition attack fully open where it
  matters most"; it is not mitigated by a selectable-set policy or a
  mandatory cross-family gate. Extends the RIG-2673 lineage ("agent acts
  as owning user, ACLs later",
  `compass-agent-org-mgmt-tools/design.md:302`) from role to profile.
  Rejected alternatives: operator-set-only, selectable-set-policy rider.
- **Model auth-fallback vs eval fidelity (was OQ-2 — RULED fail-loud, all
  sessions; Matt, 2026-08-27).** A named model that fails auth or registry
  resolution FAILS LOUD — a frame-visible HARD error, never a silent
  substitution — on ALL sessions: every session carries a profile (there is
  always a `default` profile), so the contract is universal, not
  profile-bearing-only. Compass NEVER relies on the SDK's built-in silent
  auth-fallback (`resolveModelOverrideWithAuthFallback`,
  `model-resolver.ts:1261-1297`) substituting the parent's active model.
  Mechanism note: the SDK fallback branch is gated on
  `parentActiveModelPattern` (`model-resolver.ts:1275`), but the SDK's own
  subagent runner populates `modelPatternAuthFallback` from the parent
  whenever a per-agent override is present with no concrete model
  (`executor.ts:2443-2445`), so on pinned 16.5.2 suppression is not purely
  a Compass-render matter — the implementation coupling and the T6
  obligation (a pinned SDK patch / upstream ask if no supported setting
  disables it) are recorded in T6.
- **Profile mutation (was OQ-5, deferred in the prior draft — now the
  chosen mechanism).** A profile change is applied by TEARDOWN +
  RE-PROVISION + RELAUNCH of the Manager's container with resumed session
  context: the store row is written, the container is torn down and
  re-provisioned so the Runner rebuilds a fresh `AgentHandle` from the
  updated row, then Started with resume (`COMPASS_RESUME_SESSION_FILE`
  threading exists, `agent_exec.go:92-94`). It composes existing primitives
  (despawn teardown, provision, resume) but is NOT a plain `Reload`: a
  `Reload`/`RefreshConfig` reuses the provision-captured handle spec
  (`reloadLocked` re-resolves from the registry, `host.go:857`; `agentEnv`
  reads the captured spec, `host.go:878`; no per-container Deprovision,
  `host.go:232`), so it would relaunch with the STALE profile and the store
  write would no-op (T4 specifies the orchestration). No live mid-session mutation
  path exists or is planned; the 16.5.2 snapshot-at-spawn semantics
  (§Global Constraints) make live mutation split-brain-prone anyway.
  Agent-triggered live-subtree profile mutation is DEFERRED — v1 is
  set-at-creation-only (§Global Constraints); revisit if needed (the OQ-1
  ruling covered SELECTION at spawn, not mutation of a live subtree).
- **Corpus axis vs the harness instruction-slice cut (was OQ-6 —
  reconciled).** Matt's cut of the per-agent instruction-slice field stands
  FOR THE PER-SPAWN-TUPLE SHAPE it was ruled on: no per-spawn instruction
  dial, shared-corpus-first (frozen harness ledger DL-063). The profile
  superset's `corpus` axis is a FRESH, Compass-owned, schema'd-but-deferred
  design — operator-authored profile contents selecting from the shared
  VC'd corpus at profile granularity, not a per-spawn free variable — and
  is NOT a reversal of that cut. If corpus consumption is ever activated
  (a later phase), it arrives as its own reviewed design against the
  then-current corpus posture.
- **Worker survival: ephemeral (Matt, 2026-08-27).** No Compass
  handle/account/channel for subagents; Manager relaunch resumes the
  session (completed subagent results survive in the transcript) and
  re-spawns needed workers; in-flight subagent work is lost. Accepted cost
  of the no-identity model.
- **Cross-family constraint: ADVISORY at `profiles/` PR review (reverted
  from the interim machine-lint posture by Matt's OQ-1 ruling,
  2026-08-27).** Family is explicit registry data and the profile plus its
  `models.yml` family entries land in the same reviewed bundle, so a
  reviewer can check the within-profile family pairing statically at the
  `profiles/` PR gate — but the check is reviewer-facing and advisory, not
  a door-failing lint ("cross-family stays advisory"). The across-wave
  BINDING question stays harness-owned (OQ6 cross-reference, §Cross-family
  review constraint); if the harness lands machine binding, Compass
  revisits.
- **Deferred-axis shapes are PROVISIONAL (extends the corpus ruling above to
  all three deferred axes).** `corpus`, `extensions`, and `settings` are
  schema'd for additive growth, but their concrete shapes may change without
  a schema-break ruling until first consumption — `extensions.mcp` is shaped
  against RIG-1716 (Backlog) before that contract exists, and the `settings`
  axis introduces a second overlay whose precedence against the existing
  fleet `settings/config.yml` overlay (`buildFleetSettings`, `cli.ts:497`,
  applied `cli.ts:782-785`) is DECIDED AT ACTIVATION, not committed in v1.
  Each axis activates only via its own reviewed design; consumption order
  across the three axes is likewise left to the activating phase (the schema
  is additive, so no ordering is forced; `extensions` waits on RIG-1716
  regardless).
- **Two-level UI trace is the v1 contract (designer call, folded from the
  design-critic).** The session-log trace covers the Manager plus its DIRECT
  children only; grandchild events stay on the child's private bus (§UI
  seam; `sdk.ts:1120`). The SDK-fork recursive-re-emit path is out of v1
  scope; T8 pins the boundary with a grandchild non-surfacing fixture.

## Ledger delta

Rows applied to `docs/designs/DECISIONS.md` in this freeze PR (§Agent runtime
& container), per the same-PR ledger-delta rule:

- **DL-283:** The unit of per-agent configuration is the PROFILE — a
  named, version-controlled config bundle
  (`profiles/<name>/profile.yml` in the fleet config bundle, store-door →
  mount pipeline), set per MANAGER and applied at container boot. PROFILE IS
  MANDATORY: every agent account carries one (store default `'default'`),
  there is no non-profile agent, and the committed fleet config is the
  `default` profile — which MUST exist and resolve, or the container fails
  loud at boot. This supersedes the per-spawn `{model, role}` override tuple
  of the prior RIG-2936 draft.
- **DL-284:** The Compass profile is a SUPERSET schema (models +
  system-prompt corpus + extensions/MCPs + settings), designed in full now;
  v1 populates and consumes ONLY the model-stack axis
  (`models.manager` + `models.agents`, rendered as the session
  `modelPattern` + `task.agentModelOverrides`). Deferred axes are schema'd
  for additive activation, no schema break; their shapes are PROVISIONAL
  until first consumption, and the settings-axis precedence vs
  `settings/config.yml` is decided at activation. Shared anchors with the harness
  lane (RIG-2935, FROZEN): profile concept + registry discipline, split-last-colon
  selector grammar with effort-ladder-validated thinking suffix,
  family-as-explicit-registry-data, and the semantic `manager` key; the
  schema itself is deliberately NOT shared.
- **DL-285:** Profile change = teardown + RE-PROVISION + relaunch of the
  Manager container with resumed session context (the re-provision rebuilds
  the handle from the updated store row; a plain `Reload` reuses the
  provision-captured handle and would relaunch stale — `host.go:857,878,232`);
  no live mid-session profile mutation. On the pinned SDK 16.5.2 the profile
  reaches the subagent subtree by TRANSITIVE snapshot-at-spawn
  (`createSubagentSettings` snapshots per key), not live reference — no
  executor may assume live propagation.
- **DL-286:** Worker tasks are in-process OMP subagents in the Manager's
  container — no Compass handle, account, or channel; SDK-native
  idle/park/revive lifecycle addressed by registry id over OMP-internal
  IRC; EPHEMERAL across Manager relaunch (completed results survive in the
  resumed transcript; in-flight work is lost). Compass comms tools are
  Manager-only; subagents get OMP-internal IRC only.
- **DL-287:** Subagent work surfaces in the Manager's session log as
  nested session frames — `SessionEvent.subagent_id` discriminates
  child-originated trace (empty = Manager) — with no per-subagent channel;
  the human redirects a worker by pinging the Manager. The v1 trace is
  TWO-LEVEL (Manager + direct children); grandchild events do not surface.
- **DL-288:** The profile's model axis resolves through the RIG-1715 LLM
  gateway (single egress; agents hold no provider creds) and REFERENCES the
  RIG-2845 stable-name routing vocabulary — this record delivers per-Manager
  profile SELECTION + propagation and owns model selection itself; RIG-2845
  owns the stable-name vocabulary + upstream routing the model fields name;
  RIG-1716 is where the deferred extensions axis will resolve. (Updated
  2026-08-30: RIG-2845 narrowed to routing-only — its originally-scoped role
  taxonomy was dropped, so RIG-2845 no longer "owns the policy the model
  fields name" beyond the stable-name vocabulary; role→model selection is
  this record's profile fields + OMP's built-in `modelRoles` tier defaults,
  not a RIG-2845 policy layer.)
- **DL-289:** The agent-facing spawn path may carry `profile` — a
  Manager may select its subtree's profile (RIG-2937 ruled YES,
  unconditional): no selectable-set policy, no cross-family conditioning
  (cross-family stays an advisory `profiles/` PR-review check, not a door
  lint); the oversight-degradation-by-composition residual risk is
  accepted. Owner-fence and set-at-creation-only are unchanged.
- **DL-290:** A profile-named model that fails registry resolution FAILS
  LOUD on ALL sessions (every session carries a profile; `default`
  included) — the spawn fails with a frame-visible error (the Manager belt
  `cli.ts:991-1005` fires on `session.model === undefined`). A model that
  resolves but has no working auth fails loud as a frame-visible HARD error
  (at spawn if T6 auth-pre-flights the pinned model, else at first turn),
  never a silent substitution. Compass never relies on the SDK's built-in
  silent auth-fallback substitution.
