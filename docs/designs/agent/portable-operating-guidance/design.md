# Portable operating guidance

Tracker: RIG-3903. Freezes on merge. Open decisions: RIG-3904.

## Problem / Intent

Compass already carries role prompts and several operational skills, but its portable guidance is incomplete. Port the useful operating invariants into Compass without importing Orion-specific identity, issue taxonomy, flat-peer communication, or provider credentials. Preserve the supervisor/owner/manager tree, async channel/topic model, and operator merge gate, and retain a compact survival kernel across compaction and resume.

## Approach

Keep detailed procedures in `config/skills/` and hard invariants in concise `config/rules/` files. Extend the existing `jj` and `review` skills, add provider-neutral ownership, lane, merge, CI, infrastructure, evidence, and communication guidance, and add optional adapter skills for Linear, Pulumi, and merge services. Inject a short survival kernel through the existing role-prompt/config-delivery path rather than creating a second prompt system; the kernel names the current role/tree position, issue/lane ownership, jj submission boundary, operator merge gate, evidence requirement, and safe tool-argument handling. Add a regression test at the provisioning/resume seam proving the kernel and role/persona configuration survive compaction.

## Alternatives considered

### Put all guidance in every role prompt

Rejected. It increases prompt cost, duplicates procedures, and makes provider-specific details unavoidable in every session. Only the short safety and continuity kernel needs unconditional reinjection.

### Keep all guidance in skills

Rejected. Compaction can remove recalled skill context, so hard merge, ownership, and argument-safety invariants need a durable prompt/config seam.

### Copy the Orion corpus verbatim

Rejected. Orion guidance assumes Matt, RigelBuild, RIG issue IDs, Trunk, local paths, and IRC. Compass needs provider-neutral language and its own channels/topics and tree lifecycle.

## Global Constraints

- Portable core guidance MUST NOT name Matt, RigelBuild, Orion, RIG issue IDs, Trunk, or local workspace paths.
- Preserve Compass roles (`supervisor`, `owner`, `manager`), in-process workers, async channels/topics, and operator-gated merge behavior.
- Provider-specific Linear, Pulumi, forge, and merge-queue details belong in optional adapter skills or configuration, never the portable core.
- Do not expose credentials or place secret values in prompts, rules, skills, tests, or migration notes.
- Existing generated files remain generated; prompt/config changes must use the current provisioning and config-reader seams.
- Every new operational rule must state evidence and verification expectations without adding blind retries or inert gates.

## Plan

1. Extend the existing `config/skills/jj/SKILL.md` and `config/skills/review/SKILL.md`; add focused provider-neutral skills/rules for issue ownership, delegation, lane holding, CI triage, merge boundaries, infrastructure-as-code, communication safety, and evidence discipline.
2. Add optional adapter guidance for Linear, Pulumi, and merge services with explicit provider boundaries and no credential instructions in the portable core.
3. Define the survival-kernel text and inject it through the existing role prompt/config delivery path owned by `packages/compass-agent/src/cli.ts`, `config-reader.ts`, and the provisioning/resume contract. Preserve role and persona values already delivered by `COMPASS_ROLE` and the mounted bundle.
4. Add deterministic regression coverage for provision plus compaction/resume, then run guidance lint, affected TypeScript tests, and the relevant config-delivery checks.
5. Add migration notes documenting source guidance reviewed and intentionally excluded, especially Orion flat-peer/IRC and provider-specific policy.

## Tasks

- [ ] **Port portable workflow skills**  
  **Interfaces:** consumes existing `config/skills/jj/SKILL.md`, `review/SKILL.md`, management-tree prompts, and the RIG-3903 scope; produces provider-neutral jj-vine, delegation, review/CI, lane, merge, IaC, communication, and evidence guidance with adapter boundaries.
- [ ] **Add ownership and safety rules**  
  **Interfaces:** consumes Compass issue/channel/tool semantics and existing `config/rules/own-your-issue.md`, `never-merge.md`, `never-block.md`; produces portable ownership lifecycle, safe arguments, concise async communication, and shared-failure investigation rules without replacing Compass's tree model.
- [ ] **Implement survival kernel**  
  **Interfaces:** consumes `packages/compass-agent/src/cli.ts`, `config-reader.ts`, `config/prompts/{supervisor,owner,manager}/SYSTEM.md`, `COMPASS_ROLE`, persona/config delivery, and resume seams; produces a short durable kernel that survives compaction/resume and does not create a second prompt system.
- [ ] **Add regression coverage**  
  **Interfaces:** consumes the existing provisioning/config-delivery and resume test fixtures; produces assertions for kernel retention plus role/persona preservation after compaction/resume, with no credential values.
- [ ] **Document migration boundaries**  
  **Interfaces:** consumes the reviewed portable guidance inventory and provider adapter decisions; produces concise migration notes listing omitted Orion-specific assumptions and the supported Compass extension points.

## Open Questions

- **Load-bearing:** Which existing provisioning/config-delivery artifact is the canonical durable kernel carrier: a new mounted `kernel.md` payload, a role-prompt section, or a generated field in the provision request? Recommendation: extend the existing mounted role/config bundle and config-reader seam, but confirm the exact carrier before implementation because it affects wire/config contracts and resume behavior.
- **Load-bearing:** Which provider adapters are required in this first implementation versus documented extension points? Recommendation: ship provider-neutral core plus adapter skill stubs only where Compass already has a concrete integration; do not invent provider APIs.
