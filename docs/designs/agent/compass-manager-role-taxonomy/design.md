# Compass Manager role taxonomy: supervisor / owner / manager + role-on-spawn

Status: Draft

Refs: RIG-3066 (this design pass), RIG-1724 (agent roles — the umbrella this is
the coordinator-role slice of), RIG-2673 (org-management tools — the spawn
`role`/`persona` wire field this composes with), RIG-1732 (role → block-0
mechanism). Design only — no code ships with this record.

**Frozen decisions (Matt, designed against, not relitigated):**

1. Three Manager roles — `supervisor` / `owner` / `manager` — replace today's
   single `manager` role.
2. NO worker role. Workers are subagents, never tree nodes / never mesh agents.
3. Every tree node spawns WITH a role; there is no roleless agent.
4. `supervisor` stays SPAWNABLE (an operator may stand up multiple trees). A
   spawned supervisor is parented and permitted; supervisor-with-parent is
   discouraged in docs, never server-rejected.
5. Existing-root migration is nuke-and-redeploy — the sole deployment is a
   throwaway test env; no in-place role migration is built.

This record designs HOW those land: the taxonomy contract, the role-on-spawn
mechanism and the server invariant it revises, the three role prompts' content
contract, the prompts delivery gap that must close for validation to be real,
and a new concepts doc.

**Grounding.** Citations were first verified 2026-08-31 against this clone at
`9e3f2db0`, and re-verified against current `main` (`b758bbdb`) at review. One
material change landed between those points: RIG-2726 (#762) added the
`SpawnPeerRequest.role`/`persona` wire fields, the required `role`/`persona`
tool-schema params, and the server `Role: req.GetRole()` store — so the
role-on-spawn *wire* is already built, and T1 below is scoped to what remains
(taxonomy narrowing + server closed-set validation). See §Role-on-spawn and T1.

## Problem / Intent

Compass today has ONE Manager role. `config/prompts/manager/SYSTEM.md` is the
only shipped role prompt, and the seeded root supervisor is just a manager:

> `go/server/serve_seed.go:24-28`: "role \"manager\" selects
> config/prompts/manager/SYSTEM.md as the container's block-0 prompt (RIG-1732),
> which is what makes the seeded agent a real Manager rather than a default
> agent. `const ( rootSupervisorHandle = "supervisor" …
> rootSupervisorRole = "manager" )`"

And agent-initiated spawn *originally* produced a **roleless** peer — a
default-block-0 OMP agent, deliberately. That was the pre-RIG-2726 baseline:

> `go/server/lifecycle.go` (pre-RIG-2726 #762): "Persona and role are
> server-authoritative and empty on spawn (SpawnPeerRequest carried neither):
> the new account is created with no persona and no role … `store.NewAgent{ …
> Persona: "", Role: "", … ParentAgentID: caller }`"

RIG-2726 (#762) has since landed the wire half — `SpawnPeerRequest.role = 5` /
`persona = 6`, a required `role` tool param, and the server storing
`Role: req.GetRole()` — so a spawner CAN now select a role label. But the stored
label is an unvalidated free string: there are still no `supervisor`/`owner`
prompts, no server taxonomy behind the label, and no way to express "this node
owns a product" vs "this node owns the whole tree" vs "this node drives one
lane". Matt has ruled the taxonomy (supervisor/owner/manager), ruled that every
node spawns with a validated role, and ruled that workers stay subagents. This
record turns those rulings into a buildable contract — the value-space narrowing
and server validation on top of RIG-2726's wire.

## Approach

### The taxonomy — what each role owns

Roles set **capability and posture** (via the block-0 prompt the label selects);
node **names** still state function, per the `management-trees` name-by-function
tenet ("the *role* sets capability, model, and tools; the *name* states the
function", `config/skills/management-trees/SKILL.md:72-73`). An `owner`-role
node is named "Payments" or "Infra", never "owner".

- **`supervisor` — owns the ENTIRE tree.** The root node. Receives new issues
  on the routing/intake channel; default target for alerts, notifications, and
  incidents; carries top-down broadcasts; first contact with the operator;
  spawns and owns the project subtrees. This formalizes what the
  `management-trees` skill already states informally: "Every Compass tree has
  one root-level **Supervisor** … Is the operator's first point of contact and
  posts to the top-level channels (`#announcements`, `#incidents`). Carries
  top-down broadcasts for the whole tree … Spawns and owns the project
  subtrees" (`SKILL.md:14-29`). Today the supervisor is just a `manager`
  (serve_seed.go above); this splits it out.
- **`owner` — mid-tier, owns a PRODUCT, SERVICE, or DOMAIN.** PM-flavored but
  domain-neutral: an `infra` owner is as valid as a product owner. Primary work
  is delegation and broad ownership of its area — decompose incoming issues
  into per-function work, delegate down, aggregate status up. This is the
  "Product Manager" / "department Manager" tier the skill's example trees
  already draw (`SKILL.md:81-83`: "Supervisor -> a **Product Manager** for that
  service -> function Managers beneath it … -> ephemeral workers"); the ROLE
  label is `owner`, the node name stays by-function.
- **`manager` — leaf, owns a LANE and drives it to done.** This is exactly
  today's prompt: "You are a Compass Manager. You own one lane of an agent tree
  and drive it to done" (`config/prompts/manager/SYSTEM.md:12-13`). Minimal
  churn: today's `manager/SYSTEM.md` IS the leaf role going forward.

### Why `owner` for the mid tier (alternatives considered)

- **`director` / `product-manager` / `pm`** — rejected: `director` implies
  people-management theater; `product-manager` is NOT domain-neutral (an infra
  or platform domain has no "product"). `owner` is domain-neutral AND matches
  the running wave's existing role string — wave agents are literally
  `<codename>/service-owner` — so the label carries zero retraining cost.
- **Keeping one `manager` role with a prompt that branches on tree position** —
  rejected: the role→prompt mechanism already exists per-label
  (`prompts/<role>/SYSTEM.md`); one branching mega-prompt spends block-0 tokens
  on the two positions a given node is NOT, and gives the server nothing to
  validate a spawn against.

### Why NO worker role (alternative considered and closed)

A fourth `worker` role — a tree node per implementation hand — is rejected, and
the existing subagent model is REINFORCED. This is already the shipped model:

> `config/prompts/manager/SYSTEM.md:18-20`: "Standing nodes are Managers;
> implementation runs in SUBAGENTS inside your own session — briefed by you,
> ephemeral, never tree nodes."
>
> `config/prompts/manager/SYSTEM.md:32-36`: "SUBAGENTS ARE NOT MESH NODES. A
> subagent is an in-process worker, not a peer: it has no Compass handle,
> account, or channel, and holds no Compass comms tools. … The operator has no
> channel to a worker and redirects one by pinging YOU on your channel."

Matt's rationale, which the concepts doc (T4) records first-class:

1. **No ephemeral-handle/microVM explosion.** A tree node costs a durable
   account handle and a per-agent container. Workers are numerous and
   short-scoped; minting a handle + container per worker is operationally bad.
   Subagents ride inside the Manager's existing session and container — zero
   marginal infrastructure.
2. **Comms centralization is STRUCTURAL.** A worker must not be able to talk to
   anyone besides its Manager and its sibling workers (same-session subagents,
   over OMP-internal IRC). Because a subagent holds no comms tools and has no
   channel, this is enforced by construction, not by prompt discipline — the
   same posture as the comms model's "This is **structural, not a prompting
   convention**" (`docs/concepts/comms-model.md:24-25`).
3. **Everything user-facing routes through Managers.** The operator has no
   channel to a worker; redirecting a worker means pinging its Manager. One
   legible surface per lane.

### Role-on-spawn — the mechanism, and the invariant it revises

**Tool.** `agents_spawn_peer` gains a **required** `role` parameter. Today the
wire contract is handle + display_name only:

> `packages/compass-agent/src/lifecycle.ts:77-88`: "`export const
> spawnParameters = type({ … handle: type("string") … "display_name?":
> type("string") … })`"

The schema **narrows** the existing `role` param to `role: '"supervisor" |
"owner" | "manager"'` (an arktype string-literal union, so the model sees the
closed set in the JSON schema and an off-taxonomy label is rejected at the tool
edge before any RPC). RIG-2726 (#762) already landed the wire field —
`SpawnPeerRequest.role = 5` and `persona = 6`
(`proto/compass/v1/agent_gateway.proto:201-202`), the required `role` tool param
(`packages/compass-agent/src/lifecycle.ts:85`, currently a non-blank
`type("string")`, not yet the taxonomy union), and the server store
`Role: req.GetRole()` (`go/server/lifecycle.go:227`). So this record does NOT
author a proto field or add a required param — both exist. It **narrows the
value space to the taxonomy** at the tool edge and adds the server-side
closed-set check below. No wire edit and no proto prerequisite remain.

**Server.** `SpawnAsAccount` validates `req.GetRole()` against the taxonomy and
stores it, replacing the `Role: ""` hardcode. This REVISES the
server-authoritative-empty invariant quoted in §Problem: role becomes
**caller-selected but server-validated**. Precisely:

- *What is kept:* the store remains the source of record — `provisionAndStart`
  still threads the **stored** value onto the Provision wire, and the operator
  Provision path still overwrites any client-supplied value from the store
  (`go/server/service.go:159-165`: "`if acc.IsAgent() { req.Msg.Persona =
  acc.Agent.Persona; req.Msg.Role = acc.Agent.Role } else { … = "" }`"). A
  caller still cannot inject prompt **text** — only select a label from the
  server-enforced closed set, whose text ships in the operator-published config
  bundle.
- *What changes:* the label's *origin* moves from "server hardcodes empty" to
  "spawner chooses, server validates". An unknown label → in-band
  `CodeInvalidArgument` (a tool error, per the lifecycle error contract), never
  a stored bad row.
- *Relation to org-mgmt OQ-1 ("no role allowlist").* That OQ rejected an
  **authorization** allowlist (which callers may assign which roles — "agent
  acts as owning user, ACLs later"). This record's check is a **domain**
  validation (which labels exist at all). Any owner-scoped caller may assign
  any of the three roles; the server only refuses labels outside the taxonomy.
  No conflict, and the distinction is recorded so the ledger row doesn't read
  as an OQ-1 reversal.
- *Persona:* stays mechanically orthogonal — persona APPENDS as an overlay
  while role REPLACES block-0 ("Where persona (field 3) is an APPEND overlay,
  role REPLACES block-0: the label selects
  `config/prompts/<role>/SYSTEM.md`",
  `packages/compass-agent/src/gen/compass/v1/compass_pb.ts:1110-1113`). Its
  spawn-time value follows the org-mgmt record (Matt ruled persona
  caller-supplied and required there); this record adds no persona validation
  and keeps the two channels independent (OQ-b).

**Validation source: a server constant, not a bundle walk.** The check is a
fixed `[]string{"supervisor", "owner", "manager"}` in `go/server` (beside
`rootSupervisorRole`), NOT a lookup of the current config bundle's `prompts/`
members. Rationale: the taxonomy is a frozen product decision — a closed set —
while the bundle is operator-mutable state; deriving legality from the bundle
would let an arbitrary label become spawnable by publishing a prompt dir, and
would make spawn validity flap with config pushes. The bundle-absent case is
already safe downstream: a set-but-unshipped role label falls back to the
default block-0 rather than failing ("a set-but-unshipped role FALLS BACK to
today's behavior (no customSystemPrompt)",
`packages/compass-agent/src/cli.ts:752-755`) — but that fallback is fully
SILENT today (`config-reader.ts:383-391` returns `undefined` with no log), so a
role set against a missing prompt degrades invisibly. T2 closes the delivery
gap so the skew is transitional for the first deploy cycle; steady-state, an
operator publishing a bundle that omits a prompt dir re-creates it. To keep the
server-constant decision intact while making that degradation VISIBLE, T1 adds
a compass-agent runtime warn (one `console.error`) when a role is set but
`readMountedRolePrompt` returns undefined — the cheap observability the
red-team flagged, not a blocking check.

**Path safety.** A role label is a path segment in the container; the reader
already guards traversal ("Reject a separator or `..` so the label can never
traverse outside the `prompts/` subtree … `if (/[/\\]|\.\./.test(role)) return
undefined;`", `packages/compass-agent/src/config-reader.ts:374-381`). The
closed-set server check subsumes this for spawned agents (none of the three
labels contain separators), and the reader guard stays as defense in depth.

**Spawning a `supervisor`: tree-root semantics (Matt's ruling).** All three
roles are spawnable — `spawnableRoles` keeps `supervisor`. A `supervisor` is
conceptually a tree ROOT (owns the whole tree; intake, incidents, broadcasts,
first contact), and the intended use of a spawnable supervisor is to stand up a
SEPARATE tree (an operator may want several). `SpawnAsAccount` sets
`ParentAgentID: caller` unconditionally (`lifecycle.go:212-216`), so a spawned
supervisor is technically parented; the record does NOT hard-reject a
supervisor-with-parent. Instead the concepts doc (T4) and the `supervisor`
prompt (T3) DOCUMENT that a supervisor normally heads its own tree and that
spawning one under a parent (a nested supervisor) is discouraged — but
permitted, at the operator's discretion, precisely to support multi-tree
topologies. Enforcement is guidance, not a server check. (This resolves the
red-team's supervisor-spawnability fork: keep it spawnable, doc the parent
guidance, do not enforce.)

### The prompts delivery gap this must close

Server validation "against a shipped `prompts/<role>/SYSTEM.md`" is only honest
if prompts actually ship. Today they cannot ride the config bundle: `prompts/`
is in NONE of the three bundle whitelists —

> `go/internal/store/agent_config.go:492-493` (store door): "`if
> !configBundleTopDirs[parts[0]] { return nil, fmt.Errorf("%w: bundle member %q
> is not under skills/, extensions/, mcp/, settings/, rules/, or agents/ …") }`"
>
> `go/internal/runner/config_materialize.go:70-78` (runner unpack):
> "`configTopDirs are the only permitted top-level directories in a config
> bundle` — skills/extensions/mcp/settings/rules/agents"
>
> `docs/designs/agent/compass-per-agent-overrides/design.md:134-141`
> (delivery-gap caution, verified): "`prompts/` is in NONE of the bundle
> whitelists — the store door (`agent_config.go:35-42`), the runner unpack
> (`config_materialize.go:71-78`), and the CLI bundle builder (`bundle.go:63-70`)
> … the bundle pipeline cannot deliver a `prompts/` tree today; the
> in-container reader exists, but its mount content arrives out-of-band."

The in-container reader (`readMountedRolePrompt`) is fully built and waiting;
only admission is missing. T2 adds `prompts` as a whitelisted top dir at all
three doors with the grammar `prompts/<role>/SYSTEM.md` (nested-dir shape like
`skills/<name>/…`, `<role>` under the existing safe-name regex), plus a
`Prompts` bucket so `GetAgentConfigInfo` reports shipped role prompts by name.
That info bucket is a WIRE change, not just a struct field: the result surfaces
as `GetAgentConfigInfoResponse` (`proto/compass/v1/compass.proto:752-771`,
fields 1-9) mapped in `go/server/agent_config_service.go:113-116`, so it needs
`repeated string prompts = 10` in the proto + regen + the service mapping + the
CLI render of `agent-config info` (T2 costs all four). This makes "shipped"
mean bundle-delivered, and lets the operator verify prompt presence before the
seed flip (T6).

### The three prompts — content contract (what stays, what splits out)

All three are one-screen block-0 REPLACE prompts on the existing seam
(customSystemPrompt), sharing the manager prompt's section skeleton (position /
comms / work loop) so the common Compass posture is stated once per prompt and
identically. What moves where, against today's `manager/SYSTEM.md`:

- **`manager/SYSTEM.md` (leaf) — near-stays; one addition, one conditional
  reshape.** Its identity line, coordinator-not-typist,
  subagents-are-not-mesh-nodes, async channel comms, and report-up/delegate-down
  all remain — they ARE the leaf contract. One addition: a line naming the
  three-role taxonomy and pointing at the concepts doc/skill, so a leaf knows
  what a `supervisor`/`owner` above it is. The spawn-needs-operator-approval
  gate (`SYSTEM.md:71-72`: "Spawning a child MANAGER needs OPERATOR APPROVAL
  first") stays in the baseline, but is CONDITIONALLY reshaped by T3 if OQ-a
  resolves as recommended (the bare gate becomes "a standing child usually means
  the lane should be an `owner`; propose the reshape to your parent") — see T3
  and OQ-a. So this prompt is not "nothing removed": the spawn line may change.
- **`supervisor/SYSTEM.md` (new) — the tree-owner split-out.** Identity: owns
  the entire tree, not a lane. Adds: intake — new issues land on the
  routing/intake channel and are ROUTED down, not worked; default target for
  alerts/notifications/incidents and incident coordination; top-down broadcasts
  (the `SKILL.md:25-28` "I'm going to bed" relay); first-contact-with-operator
  posture; grows/owns project subtrees (spawning `owner`s and `manager`s).
  Drops relative to the manager prompt: the own-one-lane work loop and the
  drive-issues-to-done framing (a supervisor delegates everything).
- **`owner/SYSTEM.md` (new) — the domain-owner tier.** Identity: owns one
  product/service/domain end to end. Adds: decompose area issues into
  per-function work and delegate to child `manager`s; aggregate status/PRs up
  to the supervisor; grow its own subtree (spawn child managers, same operator
  gate). Keeps the comms + subagent sections verbatim-in-spirit (an owner may
  still run subagents for area-scoped work). Drops: the single-lane framing.

Common to all three (the invariants no split may lose): coordinator-not-typist,
SUBAGENTS-ARE-NOT-MESH-NODES, async-channel-comms/never-block, operator-merges,
compact-often, and the name-by-function pointer to `skill://management-trees`.
Exact prose is implementation (T3); this contract is what T3's review checks
against.

### The concepts doc

`docs/concepts/agent-roles.md` — sibling of `comms-model.md`, matching the
style and length of the `docs/concepts/` set (architecture.md,
handle-vs-account.md, persona.md, …). Content contract (T4): the three-tier
taxonomy and what each role owns; roles-select-block-0 mechanics in one
paragraph (label → `prompts/<role>/SYSTEM.md` → customSystemPrompt REPLACE;
persona appends, orthogonal); the name-by-function composition (role ≠ name);
and the workers-as-subagents model with Matt's three rationale points (§Why NO
worker role) — framed like comms-model.md frames its split: structural, not a
prompting convention. Composes with comms-model.md explicitly: workers have
NEITHER comms surface — no channel AND no session log of their own on the mesh;
they live entirely inside a Manager's session log.

### Seed migration

`rootSupervisorRole` flips `"manager"` → `"supervisor"` (`serve_seed.go:27`),
sequenced AFTER the supervisor prompt ships in the bundle pipeline (T2+T3
before T6). The flip only affects a FIRST-LAUNCH seed on an empty tree; the
seed is idempotent by handle, so it is a no-op on an already-seeded deployment
(the root row keeps `role = "manager"`).

**Existing-root migration (Matt's ruling): nuke and redeploy.** The only live
deployment is the single-operator mattfw test env used for this program's smoke
tests; there is no production tree to preserve. So an existing root is NOT
migrated in place — the taxonomy lands, and the test env is destructively
recreated (`compass-recreate`, RIG-2660) so it re-seeds a fresh root at the new
`supervisor` default. No seed-time reconcile, no operator role-setter, no
dependency on the per-agent-overrides setter — that complexity is dropped. The
tolerant reader (`cli.ts:752-755`) still means a mis-sequenced flip degrades to
default-block-0 rather than a failed boot, but correct sequencing (T2+T3 before
T6, one release) is the plan.

## Global Constraints

- **PUBLIC repo** (RigelBuild/compass): the record, prompts, and concepts doc
  are published verbatim; no private-repo artifacts, no linked tracker URLs
  (bare RIG-NNNN per `docs/designs/CONTRIBUTING.md` rule 1).
- **Frozen taxonomy:** exactly `supervisor` / `owner` / `manager`. No worker
  role. Every spawned node carries one of the three. These are decisions, not
  parameters of any task below.
- **Role label is a path segment.** The traversal guard at
  `config-reader.ts:381` stays; the server's closed-set check makes it
  unreachable for spawned agents but it is never removed.
- **Role REPLACES block-0; persona APPENDS** (`compass_pb.ts:1110-1113`). The
  two stay orthogonal; no task couples them.
- **Store as source of record:** provision always threads the STORED
  role/persona (`service.go:159-165`); spawn-time values enter through
  `store.CreateAgent` only. The operator-Provision overwrite tests must stay
  green unchanged.
- **Name-by-function tenet preserved:** roles set capability; node names state
  function (`management-trees/SKILL.md:58-73`). Every prompt/doc/skill edit
  keeps the composition ("an `owner`-role node is named Payments, not owner").
- **Composition, not duplication, with RIG-2673:** the wire field
  (`SpawnPeerRequest.role = 5`) and the required-at-tool-schema ruling are that
  record's, and RIG-2726 (#762) has already LANDED them. This record contributes
  the value-space narrowing (non-blank string → the closed taxonomy union) +
  server closed-set validation + the three prompts, on top of that landed wire —
  no proto or tool-param authoring remains here.
- **Red-green** (rule://red-green-testing): every task lands its tests first.
- **Ledger:** `Ledger-impact:` declared in the eventual PR body; rows in
  §Ledger delta applied at freeze, not in this draft.

## Plan

### T1 — role-on-spawn: tool schema + server validation `[compass-agent + compass-server]`

Narrow and validate the spawn role end to end:

- **Landed since pin — no proto/wire work.** RIG-2726 (#762) already added
  `SpawnPeerRequest.role = 5` / `persona = 6`
  (`proto/compass/v1/agent_gateway.proto:201-202`), the required `role`/`persona`
  tool-schema params (`packages/compass-agent/src/lifecycle.ts:85,92`), and the
  server store `Role: req.GetRole()` / `Persona: req.GetPersona()`
  (`go/server/lifecycle.go:226-227`). This task authors NO proto field and adds
  NO new required param — it tightens and validates what exists.
- `packages/compass-agent/src/lifecycle.ts` — narrow the existing `role` param
  from the non-blank `type("string")` to the closed literal union
  `role: '"supervisor" | "owner" | "manager"'` with a description naming the
  taxonomy (the closed set renders into the JSON schema the model sees). The
  wire threading onto `SpawnPeerRequest.role` already exists.
- `go/server/lifecycle.go` — in `SpawnAsAccount`, add validation of
  `req.GetRole()` against a package constant `spawnableRoles =
  []string{"supervisor", "owner", "manager"}`; unknown/empty → in-band
  `CodeInvalidArgument`. All three are valid (supervisor stays spawnable per
  Matt's ruling; a spawned supervisor is parented and permitted — see §Spawning
  a supervisor). The store line already reads the caller value
  (`Role: req.GetRole()`, landed via RIG-2726); this task adds the closed-set
  guard IN FRONT of it and updates the comment block to state the revised
  invariant: role is caller-selected from the server-enforced taxonomy; prompt
  TEXT still arrives only via the operator-published bundle; the store remains
  the provision-time source of record.
- Missing-prompt observability: in compass-agent, when a role is set but
  `readMountedRolePrompt` returns undefined, emit one `console.error`
  (`config-reader.ts:383-391` is silent today) so a role-without-shipped-prompt
  degradation is visible. Non-blocking.
- Persona handling: this task touches `role` only. Persona already landed via
  RIG-2726 (required `persona` tool param + server `Persona: req.GetPersona()`
  store); this task does not narrow persona's value space (persona is free
  working-context text, not a closed taxonomy). No interim persona-less window
  remains.

Interfaces:

- Wire prerequisite — ALREADY LANDED (RIG-2726 #762): `role = 5` / `persona = 6`
  on `SpawnPeerRequest` (`proto/compass/v1/agent_gateway.proto:201-202`) and the
  regenerated `SpawnPeerRequest.GetRole()` are consumed, not authored, by this
  task.
- Consumes: `store.CreateAgent` / `store.NewAgent.Role` (accepts the label
  verbatim, `go/internal/store/inputs.go:29-32`).
- Produces: `spawnParameters` with required `role` literal-union;
  `spawnableRoles` server constant; validated `Role` on the created account;
  the compass-agent missing-prompt warn.
- Tests: TS — schema rejects missing role and off-taxonomy strings, accepts
  each of the three (extend the existing `spawnParameters` wire-contract
  tests). Go pgtest — spawn with `role: "owner"` → account row has role
  `owner` AND Provision wire carries `Role: "owner"` (pattern:
  `service_placement_pgtest_test.go` `provisionRole` asserts); spawn with
  `role: "supervisor"` → accepted (parented supervisor permitted); spawn with
  `role: "director"` → `CodeInvalidArgument`, no row; idempotent re-spawn keeps
  the stored role; `TestProvisionAgentWorkspaceOverwritesRoleFromStore` stays
  green untouched.

### T2 — `prompts/` bundle admission `[compass-server + compass-client]`

Admit `prompts/<role>/SYSTEM.md` at all three bundle doors so role prompts ship
in-band:

- `go/internal/store/agent_config.go` — add `topDirPrompts = "prompts"` to
  `configBundleTopDirs`; grammar in `validateRegularMember`: exactly
  `prompts/<role>/SYSTEM.md` (three components, `<role>` matches
  `configNamePattern`, filename exactly `SYSTEM.md`); add `Prompts []string`
  (the `<role>` names) to `AgentConfigInfoResult` + `configBundleMemberNames`.
- **Info bucket is a wire change.** `AgentConfigInfoResult` surfaces as
  `GetAgentConfigInfoResponse` (`proto/compass/v1/compass.proto:752-771`,
  fields 1-9) mapped in `go/server/agent_config_service.go:113-116`. Exposing
  `Prompts` to the operator (the whole point — "verify before the seed flip")
  needs `repeated string prompts = 10` in the proto + regen + the
  `agent_config_service.go` mapping + the `agent-config info` CLI render. Cost
  all four; a struct field alone does not reach the operator.
- `go/internal/runner/config_materialize.go` — mirror: `topDirPrompts` in
  `configTopDirs` + the nested-member grammar (structural twin discipline the
  file already documents: "kept textually in lockstep with the store door's",
  `config_materialize.go:58-59`).
- CLI bundle builder (`bundle.go` top-dir enumeration, per the overrides
  record's citation of `bundle.go:63-70`) — include `config/prompts/` when
  packing.
- No container-side change: `readMountedRolePrompt` already reads
  `prompts/<role>/SYSTEM.md` from the mount (`config-reader.ts:370-390`).

Interfaces:

- Consumes: existing bundle validate/unpack pipelines; `GetAgentConfigInfoResponse`
  proto (extended with field 10 here).
- Produces: `prompts/` admitted end to end; `AgentConfigInfoResult.Prompts` +
  the `prompts = 10` wire field; `GetAgentConfigInfo` reporting shipped
  role-prompt names through to the CLI.
- Tests: store door — `prompts/manager/SYSTEM.md` accepted; `prompts/a/b/SYSTEM.md`
  (extra depth, four components), `prompts/SYSTEM.md` (depth-2: top-level file
  directly under `prompts/`), a directory-only `prompts/manager/` entry,
  `prompts/manager/OTHER.md` (wrong filename), and `prompts/../x` (traversal)
  all rejected; Info walk returns sorted role names. Runner — unpack
  materializes `prompts/<role>/SYSTEM.md` into the version dir; escape/typeflag
  guards unchanged. E2E: put-bundle-with-prompts → runner fetch → file present
  in mount, and `agent-config info` lists the role prompt (extend
  `config_delivery_e2e_test.go`).

### T3 — the three role prompts `[config]`

Author `config/prompts/supervisor/SYSTEM.md` and `config/prompts/owner/SYSTEM.md`;
edit `config/prompts/manager/SYSTEM.md` per §content contract. The manager edit
adds the taxonomy-awareness line; it also EDITS the existing spawn-approval line
(`manager/SYSTEM.md:71-72`) if OQ-a resolves as recommended — the leaf-manager
steering copy ("a standing child usually means the lane should be an `owner`;
propose the reshape to your parent") replaces the bare approval gate, so this
task's scope is "add the taxonomy line + reshape the spawn line", not "nothing
removed". Each new prompt: one screen, the manager prompt's section skeleton,
the stays/splits allocation in §The three prompts, and every common invariant
present (coordinator-not-typist, subagents-not-mesh-nodes, async comms,
operator-merges, compact-often, `skill://management-trees` pointer). Follow the
manager prompt's `[TODO]` flip-discipline for any line gated on unlanded tools
(`SYSTEM.md:2-9`).

Interfaces:

- Consumes: §The three prompts content contract; `manager/SYSTEM.md` as the
  skeleton donor.
- Produces: `config/prompts/{supervisor,owner}/SYSTEM.md`; the edited
  `manager/SYSTEM.md`.
- Test cycle: prose deliverable — review-gated against the content contract
  (each stays/splits item checked off; grep-level check that every common
  invariant phrase appears in all three); bundle round-trip from T2's e2e test
  covers delivery.

### T4 — concepts doc `docs/concepts/agent-roles.md` `[docs]`

Write the doc per §The concepts doc: taxonomy, role mechanics (one paragraph,
citing the replace-vs-append split), name-by-function composition, and the
workers-as-subagents model with the three rationale points, framed as
structural. Cross-link comms-model.md (both directions: add one line to
comms-model.md's surface list noting subagents hold neither surface — a doc
addition, not a rewrite of existing prose).

Interfaces:

- Consumes: §Why NO worker role; §The taxonomy; `docs/concepts/comms-model.md`
  style/length as the template.
- Produces: `docs/concepts/agent-roles.md`; a one-line cross-reference in
  `comms-model.md`.
- Test cycle: prose — review-gated; link check (relative links resolve).

### T5 — `management-trees` skill update `[config]`

Update `config/skills/management-trees/SKILL.md` to name the formalized roles:
the always-a-root-Supervisor section states the root's ROLE is `supervisor`;
the example trees annotate each node with its role label (Supervisor
[`supervisor`] → Product Manager [`owner`] → CI Manager [`manager`] → workers
[subagents]); the roles-compose-with-names parenthetical (`SKILL.md:72-73`)
expands into a short subsection stating the closed set. No structural rewrite —
the skill already draws this shape informally; this makes the labels explicit.

Interfaces:

- Consumes: §The taxonomy; the existing SKILL.md text.
- Produces: the updated SKILL.md.
- Test cycle: prose — review-gated; the name-by-function tenet text is
  preserved verbatim (constraint check).

### T6 — seed flip `[compass-server]`

Flip `rootSupervisorRole` from `"manager"` to `"supervisor"`
(`serve_seed.go:27`) and update its doc comment. **Sequenced last**: lands only
after T2 (delivery) and T3 (the supervisor prompt exists in `config/`), in the
same release the operator publishes the updated bundle (OQ-c assumption).

Interfaces:

- Consumes: T2 + T3 (a shipped `prompts/supervisor/SYSTEM.md`).
- Produces: the flipped constant.
- Tests: existing seed pgtests updated — first-launch seed row carries role
  `supervisor`; seed idempotency unchanged (re-enroll on a seeded tree does not
  rewrite the role).

### T7 — consumer-contract sweep `[compass-server + docs]`

Under the taxonomy, two doc/spec surfaces drift and must be corrected before an
implementer codes against them:

- `go/internal/linearagent/routing.go:48-54` — the `ManagerResolver` contract
  reads "walk up `parent_agent_id` to a role=`"manager"` agent". Post-taxonomy
  that is wrong twice: a `manager`-only filter SKIPS an `owner` parent (routing
  a domain-owned artifact past its owning tier), and every tree node is now a
  Manager-class node so the filter is vestigial. Correct the documented
  contract to "nearest tree ANCESTOR" (drop the role== filter). Only the
  interface + test fakes exist today (no store-backed impl is wired —
  `NewResolver` is called only from tests), so this is a doc-contract fix now
  that prevents a future implementer hardcoding `role=="manager"`.
- `proto/compass/v1/compass.proto:599` — the `role` doc comment says
  "operator-set"; after T1 the role is SPAWNER-set (server-validated). Sweep the
  comment to match.

Interfaces:

- Consumes: §The taxonomy; the two cited surfaces.
- Produces: corrected `ManagerResolver` doc contract; corrected proto comment.
- Test cycle: `routing_test.go` fakes stay green (doc/contract change, no impl
  wired); the proto comment edit is regen-neutral (comment-only).

## Tasks

- [ ] T1 `[compass-agent + compass-server]` `agents_spawn_peer` required `role`
      literal-union + `SpawnAsAccount` closed-set validation + invariant-comment
      rewrite + TS/Go tests
- [ ] T2 `[compass-server + compass-client]` `prompts/` admitted at store door,
      runner unpack, CLI builder + `Prompts` info bucket + door/unpack/e2e tests
- [ ] T3 `[config]` `prompts/{supervisor,owner}/SYSTEM.md` authored +
      `manager/SYSTEM.md` taxonomy line (content contract §The three prompts)
- [ ] T4 `[docs]` `docs/concepts/agent-roles.md` + comms-model.md cross-link
- [ ] T5 `[config]` `management-trees` SKILL.md role labels formalized
- [ ] T6 `[compass-server]` `rootSupervisorRole` → `"supervisor"` (after T2+T3)
      + seed tests
- [ ] T7 `[compass-server + docs]` `ManagerResolver` doc contract →
      nearest-ancestor + `compass.proto:599` role-comment sweep

## Ledger delta

Ledger-impact: applied at freeze, not in this draft. Rows to add:

- **DL-new-A:** The Compass Manager taxonomy is exactly three roles —
  `supervisor` (owns the tree; intake, incidents, broadcasts, first contact),
  `owner` (owns a product/service/domain; delegation + area ownership),
  `manager` (owns a lane; drives it to done). Roles set capability; node names
  still state function (composes with the name-by-function tenet).
- **DL-new-B:** Every tree node spawns WITH a role: `agents_spawn_peer` carries
  a required role from the closed taxonomy, server-validated
  (`CodeInvalidArgument` on unknown). REVISES the server-authoritative-empty
  spawn invariant (lifecycle.go): role is caller-selected-but-server-validated;
  prompt text remains operator-bundle-only; store remains provision source of
  record. Domain validation, not an authz allowlist — does not reverse
  RIG-2673 OQ-1.
- **DL-new-C:** NO worker role. Workers are subagents inside a Manager's
  session — no handle, no container, no comms tools — enforcing centralized
  comms structurally; all user-facing traffic routes through Managers.
- **DL-new-D:** `prompts/<role>/SYSTEM.md` is a whitelisted config-bundle
  member at all three doors (store, runner, builder), closing the
  out-of-band-prompts delivery gap.
- **DL-new-E:** `supervisor` is agent-spawnable (multi-tree topologies);
  supervisor-with-parent is doc-discouraged, not server-rejected.
  Existing-root migration is nuke-and-redeploy (single test env), not an
  in-place role migration.
- **DL-new-F:** `ManagerResolver` routing walks to the NEAREST tree ancestor
  (any Manager-class role), not to a `role=="manager"` node — every node is now
  Manager-class, and an `owner` parent must not be skipped.

## Open Questions

Batched for Matt's single ask; each is designed-against with the stated
assumption, none blocks the tasks above.

- **OQ-a — Can a leaf `manager` spawn child Managers, or do only
  `supervisor`/`owner` grow the tree?** *Designed against:* any Manager may
  spawn, gated by operator approval exactly as today
  (`manager/SYSTEM.md:71-72`) — the approval gate already puts a human on every
  tree-growth event, so a role-based server restriction adds enforcement
  without adding safety. *Recommendation:* keep spawn universal at the server;
  steer in prompt copy instead — the leaf `manager` prompt says "if your lane
  needs a standing child, that is usually a sign the lane should be an `owner`;
  propose the reshape to your parent" rather than hard-blocking. Revisit a
  server-side restriction only if approval fatigue makes the gate rubber-stamp.
- **OQ-b — Does persona stay orthogonal to role?** *Designed against:* yes,
  unchanged — persona appends, role replaces (`compass_pb.ts:1110-1113`);
  spawn-time persona follows RIG-2673's ruling independently.
  *Recommendation:* confirm; no task here touches persona semantics.
- **OQ-c — Seed migration. RESOLVED (Matt): nuke and redeploy.** The live
  deployment is a single-operator test env with no tree to preserve, so an
  existing root is not migrated in place — the taxonomy lands, the test env is
  destructively recreated (`compass-recreate`) and re-seeds a fresh
  `supervisor` root. Seed-time reconcile and an operator role-setter are both
  dropped; no dependency on the per-agent-overrides setter. Sequencing stands:
  T2+T3 (prompt shipped) before T6 (seed flip), one release. See §Seed
  migration.
- **OQ-d — Does comms centralization need hardening, or is it purely
  structural?** *Designed against:* structural and already enforced — subagents
  hold no comms tools and no handle (`manager/SYSTEM.md:32-36`), so
  worker-reaches-only-its-manager is true by construction; T4 documents it, no
  code change. *Recommendation:* doc-only now; the one seam worth a later
  audit is tool inheritance if subagent tool-surfaces ever widen (a subagent
  must never inherit its Manager's comms/lifecycle tools).
