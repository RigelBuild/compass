# Portable operating guidance

Tracker: RIG-3903. Freezes on merge.

## Problem / Intent

Compass already carries role prompts and several operational skills, but its portable guidance is incomplete. The default bundle was frozen by [compass-batteries-included](../compass-batteries-included/design.md) (RIG-1738, DL-140..DL-147); guidance added to the source wave's corpus since that freeze has not been carried across. Port the remaining operating invariants without importing the source wave's identity, issue taxonomy, flat-peer communication, or provider credentials. Preserve the supervisor/owner/manager tree, async channel/topic model, and operator merge gate, and retain a compact survival kernel across compaction and resume.

## Approach

Keep detailed procedures in `config/skills/` and hard invariants in concise `config/rules/` files, inside the structure [compass-batteries-included](../compass-batteries-included/design.md) froze. Extend the existing `jj` and `review` skills and add provider-neutral ownership, lane, merge, CI, infrastructure, evidence, and communication guidance. Ship a portable core with adapter extension points; this change adds no concrete provider adapters. The survival kernel is an `alwaysApply: true` rule delivered through `config-reader.ts` into `createAgentSession({ rules })`, following `never-merge.md`. Keep only role-invariant evidence requirements and safe tool-argument handling in the kernel. The kernel cites DL-142 for version-control guidance instead of restating it; other invariants remain in their existing or planned homes.

## Alternatives considered

### Put all guidance in every role prompt

Rejected. It increases prompt cost, duplicates procedures, and makes provider-specific details unavoidable in every session. The short, role-invariant safety kernel needs unconditional delivery on every turn.

### Keep all guidance in skills

Rejected. Compaction can remove recalled skill context, so hard invariants need a durable prompt/config seam.

### Copy the source corpus verbatim

Rejected, and already ruled at BC-2 (adapt, don't fork — RIG-1732 GC-7). The source guidance assumes a specific human, forge organization, issue-key prefix, merge service, local paths, and a flat peer mesh. Compass needs provider-neutral language plus its own channels/topics and tree lifecycle.

### Re-open the batteries manifest

Rejected. DL-141 (delegated-implementation folds into management-trees) and DL-142 (version-control's footguns fold into one always-apply rule) are frozen placement decisions. This record adds guidance within that structure and supersedes by citation where a decision genuinely moves, never by rewriting the manifest.

## Global Constraints

- Portable core guidance MUST NOT name a specific human, a private repository, the forge organization, a specific issue-key prefix, a named merge service, or local workspace paths; refer to the human generically as "the operator". This repo is public: `tools/orion-ref-gate` runs on PRs (its `check` task takes the whole tree as input, so `moon query tasks --affected` selects it), but it matches only the private repo's two names (the current one as a whole word, the former one in repo-shaped forms), so a tokenless path citation or place-pointer rests on the author (see `docs/concepts/self-host-and-managed.md`).
- Preserve Compass roles (`supervisor`, `owner`, `manager`), in-process workers, async channels/topics, and operator-gated merge behavior.
- Provider-specific Linear, Pulumi, forge, and merge-queue details belong in operator user configuration or later skill bundles, never the portable core. This change defines extension points but adds no concrete adapters.
- Do not expose credentials or place secret values in prompts, rules, skills, tests, or migration notes.
- Existing generated files remain generated; prompt/config changes must use the current provisioning and config-reader seams.
- Every new operational rule must state evidence and verification expectations without adding blind retries or inert gates.

## Plan

1. Extend the existing `config/skills/jj/SKILL.md` and `config/skills/review/SKILL.md`; add focused provider-neutral skills/rules for issue ownership, delegation, lane holding, CI triage, merge boundaries, infrastructure-as-code, communication safety, and evidence discipline.
2. Keep provider-specific material out of this change. Define adapter extension points; later skill bundles may carry specific guidance, while operators can use their own configuration in the meantime.
3. Write the kernel as an `alwaysApply: true` rule in `config/rules/`. Always-apply rules live in the system prompt, and compaction rewrites only the message history, so the kernel survives compaction with no reinjection seam (see Resolved decisions). Preserve role (`COMPASS_ROLE` selects `prompts/<role>/SYSTEM.md`, injected as `customSystemPrompt`) and persona (`COMPASS_PERSONA`, appended via `systemPrompt`).
4. Add deterministic regression coverage for provision plus compaction/resume, then run the root `markdownlint` task, affected TypeScript tests, and the relevant config-delivery checks.
5. Add migration notes recording which source guidance was reviewed and deliberately not carried — the flat-peer/IRC model and provider-specific policy — cited alongside DL-143's frozen exclusion set, with any new exclusion needing ratification filed as its own ledger row.

## Tasks

- [ ] **Port portable workflow skills**  
  **Interfaces:** consumes existing `config/skills/jj/SKILL.md`, `review/SKILL.md`, management-tree prompts, and the RIG-3903 scope; produces provider-neutral jj-vine, delegation, review/CI, lane, merge, IaC, communication, and evidence guidance with adapter boundaries.
- [ ] **Add ownership and safety rules**  
  **Interfaces:** consumes Compass issue/channel/tool semantics and existing `config/rules/own-your-issue.md`, `never-merge.md`, `never-block.md`; produces portable ownership lifecycle, safe arguments, concise async communication, and shared-failure investigation rules without replacing Compass's tree model.
- [ ] **Implement survival kernel**  
  **Interfaces:** consumes `readMountedRules` in `packages/compass-agent/src/config-reader.ts` (flat `rules/*.md`/`*.mdc`, `readdir` plus extension filter, not a glob), the rules composition in `main` in `packages/compass-agent/src/cli.ts` (`const rules = [...mounted.rules, ...discoveredRules];`), and the existing `alwaysApply: true` convention in `config/rules/never-merge.md`; produces one short always-apply kernel rule containing only role-invariant evidence requirements and safe tool-argument handling, citing DL-142 for version-control guidance. No reinjection seam.
- [ ] **Add regression coverage**  
  **Interfaces:** consumes the existing provisioning/config-delivery and resume test fixtures; produces independent assertions with no credential values: (1) the kernel is retained after compaction/resume; (2) role/persona values survive compaction/resume; (3) a subagent-shaped session (`taskDepth: 1`, as in `subagent-tool-split.test.ts`) built with the composed rules receives the kernel rule. Whether the body is correct for both roles (BC-7) is a review check, not a test.
- [ ] **Document migration boundaries**  
  **Interfaces:** consumes the reviewed portable guidance inventory, DL-143's frozen exclusion set (cited, never extended in place), and the provider adapter decisions; produces concise migration notes naming the omitted source-wave assumptions and the supported Compass extension points, with no private-repo references.

## Resolved decisions

- **RIG-3904 Q1 — kernel carrier:** Use an `alwaysApply: true` rule in `config/rules/`. `main` in `packages/compass-agent/src/cli.ts` passes `const rules = [...mounted.rules, ...discoveredRules];` to `createAgentSession({ rules })`. In the pinned SDK (`@oh-my-pi/pi-coding-agent` 18.0.11), `bucketRules` in `src/capability/rule-buckets.ts` sends `alwaysApply === true` rules to `alwaysApplyRules`, and `src/prompts/system/custom-system-prompt.md` renders them into the system prompt (`{{#each alwaysApplyRules}}`), the path a `customSystemPrompt` role takes. Compaction rewrites messages and never writes `agent.state.systemPrompt`; the SDK's own prompt writers (turn-start reset, `refreshBaseSystemPrompt`, rollback) rebuild from or restore the same rules. A resumed agent is a new process, and `main` rebuilds the prompt from the same rules. So the kernel survives compaction and resume with no reinjection seam; kernel test (1) pins it. One caveat: the `config` control in `#applyControl` (`packages/compass-agent/src/agent.ts`) calls `setSystemPrompt(control.systemPrompt)` and would replace the kernel. No production code sends it today; a future sender must carry the kernel.
- **RIG-3904 Q2 — adapters:** Ship portable core guidance with adapter extension points only. Add no concrete adapters in this change. Later skill bundles may carry provider-specific material; until then, operators use their own configuration.
- **RIG-3926 — kernel content:** Keep only role-invariant evidence requirements and safe tool-argument handling in the kernel. Leave other guidance in its existing or planned homes. Cite DL-142 for the version-control rule rather than restating it. Accept the gap until B1/B2/B7 ship; do not wait for those items.

DL-144/BC-7 requires role self-scoping for forwarded always-apply rules; the selected kernel items are role-invariant. No role-specific operator or manager affordances belong in the kernel.
