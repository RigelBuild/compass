# Portable operating guidance

Tracker: RIG-3903. Freezes on merge. Open decisions: RIG-3904.

## Problem / Intent

Compass already carries role prompts and several operational skills, but its portable guidance is incomplete. The default bundle was frozen by [compass-batteries-included](../compass-batteries-included/design.md) (RIG-1738, DL-140..DL-147); guidance added to the source wave's corpus since that freeze has not been carried across. Port the remaining operating invariants without importing the source wave's identity, issue taxonomy, flat-peer communication, or provider credentials. Preserve the supervisor/owner/manager tree, async channel/topic model, and operator merge gate, and retain a compact survival kernel across compaction and resume.

## Approach

Keep detailed procedures in `config/skills/` and hard invariants in concise `config/rules/` files, inside the structure [compass-batteries-included](../compass-batteries-included/design.md) froze. Extend the existing `jj` and `review` skills, add provider-neutral ownership, lane, merge, CI, infrastructure, evidence, and communication guidance, and add optional adapter skills for Linear, Pulumi, and merge services. The survival kernel rides the mechanism Compass already has rather than a new one: an `alwaysApply: true` rule delivered through `config-reader.ts` into `createAgentSession({ rules })`, the same path `never-merge.md` uses. The kernel names the current role/tree position, issue/lane ownership, the jj submission boundary, the operator merge gate, the evidence requirement, and safe tool-argument handling. Whether that injection alone survives an in-session compaction is the first open question and is settled by reading the SDK compaction path, not by adding a seam speculatively.

## Alternatives considered

### Put all guidance in every role prompt

Rejected. It increases prompt cost, duplicates procedures, and makes provider-specific details unavoidable in every session. Only the short safety and continuity kernel needs unconditional reinjection.

### Keep all guidance in skills

Rejected. Compaction can remove recalled skill context, so hard merge, ownership, and argument-safety invariants need a durable prompt/config seam.

### Copy the source corpus verbatim

Rejected, and already rejected once at DL-143 (the wave-specific exclusion set). The source guidance assumes a specific human, forge organization, issue-key prefix, merge service, local paths, and a flat peer mesh. Compass needs provider-neutral language plus its own channels/topics and tree lifecycle.

### Re-open the batteries manifest

Rejected. DL-141 (delegated-implementation folds into management-trees) and DL-142 (version-control's footguns fold into one always-apply rule) are frozen placement decisions. This record adds guidance within that structure and supersedes by citation where a decision genuinely moves, never by rewriting the manifest.

## Global Constraints

- Portable core guidance MUST NOT name the private monorepo, the operator, the forge organization, a specific issue-key prefix, a named merge service, or local workspace paths. This repo is public and `tools/orion-ref-gate` fails closed on private-repo references.
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
5. Add migration notes recording which source guidance was reviewed and deliberately not carried — the flat-peer/IRC model and provider-specific policy — extending DL-143's exclusion set rather than restating it.

## Tasks

- [ ] **Port portable workflow skills**  
  **Interfaces:** consumes existing `config/skills/jj/SKILL.md`, `review/SKILL.md`, management-tree prompts, and the RIG-3903 scope; produces provider-neutral jj-vine, delegation, review/CI, lane, merge, IaC, communication, and evidence guidance with adapter boundaries.
- [ ] **Add ownership and safety rules**  
  **Interfaces:** consumes Compass issue/channel/tool semantics and existing `config/rules/own-your-issue.md`, `never-merge.md`, `never-block.md`; produces portable ownership lifecycle, safe arguments, concise async communication, and shared-failure investigation rules without replacing Compass's tree model.
- [ ] **Implement survival kernel**  
  **Interfaces:** consumes `packages/compass-agent/src/config-reader.ts` (flat `rules/*.md` glob, `createAgentSession({ rules })` short-circuit), the existing `alwaysApply: true` convention in `config/rules/never-merge.md`, `COMPASS_ROLE`, and the SDK compaction path; produces one short always-apply kernel rule, and a reinjection seam only if the compaction read proves one is required.
- [ ] **Add regression coverage**  
  **Interfaces:** consumes the existing provisioning/config-delivery and resume test fixtures; produces assertions for kernel retention plus role/persona preservation after compaction/resume, with no credential values.
- [ ] **Document migration boundaries**  
  **Interfaces:** consumes the reviewed portable guidance inventory, DL-143's frozen exclusion set, and the provider adapter decisions; produces concise migration notes naming the omitted source-wave assumptions and the supported Compass extension points, with no private-repo references.

## Open Questions

- **Load-bearing:** Whether an `alwaysApply: true` rule is sufficient as the kernel carrier, or whether the kernel additionally needs a post-compaction reinjection point. Compass already delivers always-apply rules — `config-reader.ts` globs flat `rules/*.md`, and providing the array to `createAgentSession({ rules })` short-circuits SDK rule discovery so the fleet set is the sole injected set (`config-reader.ts:402-439`); `never-merge.md` already carries `alwaysApply: true`. So the carrier exists and no new prompt system is needed. What is NOT yet grounded is whether that injection survives a compaction within a live session, or only re-applies at session construction. Recommendation: settle this by reading the SDK's compaction path before writing the kernel — if always-apply rules are re-applied post-compaction, the kernel is a rule file and the remaining work is purely editorial; if not, it needs a reinjection seam and the task grows. Do not design the seam before that read.
- **Load-bearing:** Which provider adapters are required in this first implementation versus documented extension points? Recommendation: ship provider-neutral core plus adapter skill stubs only where Compass already has a concrete integration; do not invent provider APIs.
