# Portable operating guidance

Tracker: RIG-3903. Freezes on merge. Open decisions: RIG-3904.

## Problem / Intent

Compass already carries role prompts and several operational skills, but its portable guidance is incomplete. The default bundle was frozen by [compass-batteries-included](../compass-batteries-included/design.md) (RIG-1738, DL-140..DL-147); guidance added to the source wave's corpus since that freeze has not been carried across. Port the remaining operating invariants without importing the source wave's identity, issue taxonomy, flat-peer communication, or provider credentials. Preserve the supervisor/owner/manager tree, async channel/topic model, and operator merge gate, and retain a compact survival kernel across compaction and resume.

## Approach

Keep detailed procedures in `config/skills/` and hard invariants in concise `config/rules/` files, inside the structure [compass-batteries-included](../compass-batteries-included/design.md) froze. Extend the existing `jj` and `review` skills, add provider-neutral ownership, lane, merge, CI, infrastructure, evidence, and communication guidance, and add optional adapter skills for Linear, Pulumi, and merge services. The survival kernel rides the mechanism Compass already has rather than a new one: an `alwaysApply: true` rule delivered through `config-reader.ts` into `createAgentSession({ rules })`, the same path `never-merge.md` uses. Candidate kernel content is the role/tree position, issue/lane ownership, the jj submission boundary, the operator merge gate, the evidence requirement, and safe tool-argument handling — but that list is **not settled**: DL-144/BC-7 role self-scoping and BC-6 terseness constrain what one always-apply rule may carry, which is the third open question. Whether always-apply injection alone survives an in-session compaction is the first, settled by reading the SDK compaction path rather than adding a seam speculatively.

## Alternatives considered

### Put all guidance in every role prompt

Rejected. It increases prompt cost, duplicates procedures, and makes provider-specific details unavoidable in every session. Only the short safety and continuity kernel needs unconditional delivery on every turn; whether that requires reinjection beyond always-apply injection is OQ-1.

### Keep all guidance in skills

Rejected. Compaction can remove recalled skill context, so hard merge, ownership, and argument-safety invariants need a durable prompt/config seam.

### Copy the source corpus verbatim

Rejected, and already ruled at BC-2 (adapt, don't fork — RIG-1732 GC-7). The source guidance assumes a specific human, forge organization, issue-key prefix, merge service, local paths, and a flat peer mesh. Compass needs provider-neutral language plus its own channels/topics and tree lifecycle.

### Re-open the batteries manifest

Rejected. DL-141 (delegated-implementation folds into management-trees) and DL-142 (version-control's footguns fold into one always-apply rule) are frozen placement decisions. This record adds guidance within that structure and supersedes by citation where a decision genuinely moves, never by rewriting the manifest.

## Global Constraints

- Portable core guidance MUST NOT name a specific human, a private repository, the forge organization, a specific issue-key prefix, a named merge service, or local workspace paths; refer to the human generically as "the operator". This repo is public: `tools/orion-ref-gate` runs on PRs (its `check` task takes the whole tree as input, so `moon query tasks --affected` selects it), but it matches one literal identifier only, so a tokenless path citation or place-pointer rests on the author (see `docs/concepts/self-host-and-managed.md`).
- Preserve Compass roles (`supervisor`, `owner`, `manager`), in-process workers, async channels/topics, and operator-gated merge behavior.
- Provider-specific Linear, Pulumi, forge, and merge-queue details belong in optional adapter skills or configuration, never the portable core.
- Do not expose credentials or place secret values in prompts, rules, skills, tests, or migration notes.
- Existing generated files remain generated; prompt/config changes must use the current provisioning and config-reader seams.
- Every new operational rule must state evidence and verification expectations without adding blind retries or inert gates.

## Plan

1. Extend the existing `config/skills/jj/SKILL.md` and `config/skills/review/SKILL.md`; add focused provider-neutral skills/rules for issue ownership, delegation, lane holding, CI triage, merge boundaries, infrastructure-as-code, communication safety, and evidence discipline.
2. Add optional adapter guidance for Linear, Pulumi, and merge services with explicit provider boundaries and no credential instructions in the portable core.
3. Write the kernel as an `alwaysApply: true` rule in `config/rules/`, after confirming against the SDK compaction path whether always-apply rules re-apply post-compaction. Add a reinjection seam only if that read shows they do not. Preserve role and persona values already delivered by `COMPASS_ROLE` and the mounted bundle.
4. Add deterministic regression coverage for provision plus compaction/resume, then run guidance lint, affected TypeScript tests, and the relevant config-delivery checks.
5. Add migration notes recording which source guidance was reviewed and deliberately not carried — the flat-peer/IRC model and provider-specific policy — cited alongside DL-143's frozen exclusion set, with any new exclusion needing ratification filed as its own ledger row.

## Tasks

- [ ] **Port portable workflow skills**  
  **Interfaces:** consumes existing `config/skills/jj/SKILL.md`, `review/SKILL.md`, management-tree prompts, and the RIG-3903 scope; produces provider-neutral jj-vine, delegation, review/CI, lane, merge, IaC, communication, and evidence guidance with adapter boundaries.
- [ ] **Add ownership and safety rules**  
  **Interfaces:** consumes Compass issue/channel/tool semantics and existing `config/rules/own-your-issue.md`, `never-merge.md`, `never-block.md`; produces portable ownership lifecycle, safe arguments, concise async communication, and shared-failure investigation rules without replacing Compass's tree model.
- [ ] **Implement survival kernel**  
  **Interfaces:** consumes `readMountedRules` in `packages/compass-agent/src/config-reader.ts` (flat `rules/*.md`/`*.mdc`, `readdir` plus extension filter, not a glob), the rules composition in `main` in `packages/compass-agent/src/cli.ts` (`const rules = [...mounted.rules, ...discoveredRules];`), the existing `alwaysApply: true` convention in `config/rules/never-merge.md`, `COMPASS_ROLE`, and the SDK compaction path; produces one short always-apply kernel rule whose body self-scopes by role per DL-144/BC-7, and a reinjection seam only if the compaction read proves one is required.
- [ ] **Add regression coverage**  
  **Interfaces:** consumes the existing provisioning/config-delivery and resume test fixtures; produces assertions for kernel retention plus role/persona preservation after compaction/resume, with no credential values.
- [ ] **Document migration boundaries**  
  **Interfaces:** consumes the reviewed portable guidance inventory, DL-143's frozen exclusion set (cited, never extended in place), and the provider adapter decisions; produces concise migration notes naming the omitted source-wave assumptions and the supported Compass extension points, with no private-repo references.

## Open Questions

- **Load-bearing:** Whether an `alwaysApply: true` rule is sufficient as the kernel carrier, or whether the kernel additionally needs a post-compaction reinjection point. Compass already delivers always-apply rules: `readMountedRules` in `packages/compass-agent/src/config-reader.ts` reads the mounted fleet set, and `main` in `packages/compass-agent/src/cli.ts` composes it with the checkout's discovered rules — `const rules = [...mounted.rules, ...discoveredRules];`, fleet first and deliberately least-prominent — passing the composed array to `createAgentSession({ rules })`, which short-circuits the SDK's own discovery. `never-merge.md` already carries `alwaysApply: true`. So the carrier exists and no new prompt system is needed. What is NOT grounded is whether that injection survives a compaction within a live session, or only re-applies at session construction. Recommendation: settle it by reading the SDK compaction path before writing the kernel — if always-apply rules re-apply post-compaction the kernel is a rule file and the rest is editorial; if not, it needs a reinjection seam and the task grows. Do not design the seam before that read.
- **Load-bearing:** Which provider adapters are required in this first implementation versus documented extension points? Recommendation: ship provider-neutral core plus adapter skill stubs only where Compass already has a concrete integration; do not invent provider APIs.
- **Load-bearing (routed to the operator, RIG-3926):** What the kernel may contain, given DL-144/BC-7. DL-144 rules that every forwarded always-apply rule self-scopes by role, because the `task` executor forwards parent session rules into every subagent but not the session `customTools` — so a Manager always-apply rule rides every implementer-subagent turn without the affordances it assumes, and `hold-your-lane`, `decision-authority`, and `own-your-issue` are named as actively wrong there. At least three of the kernel items sketched in Approach (role/tree position, issue/lane ownership, the operator merge gate) are exactly those Manager-only affordances. BC-6 binds too: always-apply stays terse, "screens-of-one, not screens-of-three", and one rule carrying six invariants is the shape it forbids. Options: (i) keep all six and branch the body by role per BC-7; (ii) keep only the role-invariant items (jj submission boundary, evidence, safe tool arguments) and leave the Manager items to the role-aware rules B2/B7 already specify; (iii) record why a single unbranched kernel is acceptable despite DL-144. Recommendation: (ii), because it satisfies BC-6 terseness without asking one rule body to be correct in two roles. **Do not implement the kernel before this is ruled.**
