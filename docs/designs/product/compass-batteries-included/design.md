# Compass "batteries included" — the default skill/rule bundle (Dogfood cut)

Status: Draft

> Freezes on merge; later changes supersede by citation, never rewrite.
> Tracked as SEA-1738. Fast-follow to the SEA-1732 Manager+implementer prompt
> record ([compass-manager-prompt](../compass-manager-prompt/design.md), PR
> #1089): #1089 freezes the two role prompts (Manager block-0 + implementer
> block-0, MP-1..MP-5 / DL-129..DL-134); THIS record freezes the batteries
> manifest around them — which skills and rules ship by default, per role, and
> how each was adapted from the current wave's `~/.agents/{skills,rules}`.
>
> New named Decisions this record introduces (ledger rows DL-140..DL-147,
> landed in DECISIONS.md in this same PR): **DL-140** (language-neutral default
> bundle; Go pack deferred to SEA-1739/Beta), **DL-141**
> (delegated-implementation folds into management-trees, no separate skill),
> **DL-142** (version-control's three footguns fold into one Compass
> always-apply rule), **DL-143** (the wave-specific exclusion set), **DL-144**
> (forwarded always-apply rules self-scope by role — BC-7), **DL-145** (B1's
> `version-control` fold hard-ordered after T8, no `[TODO T8]` in an
> always-apply rule), **DL-146** (process-safety ships at rulebook tier, not
> always-apply — per-container isolation removes the shared-box clobber blast
> radius), **DL-147** (the default batteries are stack-neutral, not merely
> language-neutral — devenv/direnv is a kept product baseline, GitHub-Actions
> specifics leave `ci-failure-triage` for SEA-1739 pack territory).
>
> Grounding: every delivery-mechanism claim below was re-verified firsthand
> against the compass repo at **`origin/main = cf048ca`** (read-only checkout,
> 2026-08-05); every source-skill/rule claim against the wave's live
> `~/.agents/{skills,rules}` as of 2026-08-05. The manifest decisions are
> Matt-ratified (2026-08-05, "add all recs" + 6 refinements, resolved in
> `compass-batteries-included.md`); this record records them, it does not
> re-open them.

## Problem / Intent

A freshly-spawned Compass agent gets the two role prompts (SEA-1732: Manager
block-0 via `customSystemPrompt`, implementer block-0 as a mounted
`config/agents/` subagent def) — but **no skills and no rules**. The prompt
tells it *what it is*; the batteries tell it *how work is done here*: the jj
stacked-PR workflow, the review loop, CI triage, issue ownership, the
never-block/never-merge invariants. The current wave carries all of this as
Matt's `~/.agents/{skills,rules}` — a set that is partly universal discipline
and partly wave-specific infrastructure (zellij fleets, Woodpecker CI, the
supervisor mesh) that would actively confuse a Compass agent.

This record freezes the **default bundle manifest** for the Dogfood cut: per
role (Manager, Implementer), which rules and skills ship, where each comes
from (folded / adapted / authored new), and what is deliberately excluded and
why. Scope boundaries, both Matt-ratified: the default is **language-neutral**
(no Go pack — that is the SEA-1739 "packs" feature, Beta; for Dogfood,
Matt-as-user adds his Go skills via the normal user-skill path), and this is a
**manifest + adaptation-decision record** — the skill/rule BODIES are authored
in the impl children below, matching the SEA-1732 T4–T9 pattern.

## Approach

### Decision BI-1 — two role bundles on one delivery surface

Two roles, two bundles, matching the SEA-1732 cost split (MP-2/DL-130):

- **Manager** — coordinator. Small always-on surface: the ~500-word block-0
  (SEA-1732 T1) + always-apply rules (full text every turn) + on-demand
  skills (name + one-liner every turn, body via `skill://`).
- **Implementer** — hands, an in-process `task` subagent (MP-5/DL-134). Large
  block-0 (SEA-1732 T2, `config/agents/implementer.md`) + domain rules
  (rulebook tier: loaded on demand by description match).

**How the bundle is consumed — grounded at `cf048ca`.** The batteries ship as
members of the config bundle SEA-1678's passthrough delivers (skills, rules,
agents, AGENTS.md), and the compass entrypoint maps each member onto a
`createAgentSession` seam:

- **Skills** are read from the mount's `skills/` tree and injected as the
  sole skill source — `packages/compass-agent/src/config-reader.ts:84-89`:

  ```ts
  const { skills } = await loadSkillsFromDir({
      dir: join(currentDir, "skills"),
      source: SKILL_SOURCE,
  });
  ```

  and passed unconditionally (`packages/compass-agent/src/cli.ts:630`,
  `skills: mounted.skills`), which skips SDK discovery entirely — the fork's
  seam at `forks/oh-my-pi/packages/coding-agent/src/sdk.ts:1507-1508`:

  ```ts
  if (options.skills !== undefined) {
      skills = options.skills;
  ```

- **Rules** are read from the mount's flat `rules/*.md|*.mdc` and COMPOSED
  fleet-first with the checkout's discovered rules —
  `config-reader.ts:376-404` (`readMountedRules`, built "with the SDK's own
  `buildRuleFromMarkdown` … so frontmatter (globs, alwaysApply, description,
  conditions) is parsed identically", `config-reader.ts:371-373`), then
  `cli.ts:606`:

  ```ts
  const rules = [...mounted.rules, ...discoveredRules];
  ```

- **Rule tiers are frontmatter-driven**, so the manifest's always-apply vs
  domain split is carried entirely by each rule file's `alwaysApply: true` /
  `description:` frontmatter. The fork buckets every rule in one funnel —
  `forks/oh-my-pi/packages/coding-agent/src/capability/rule-buckets.ts:56-62`:

  ```ts
  if (rule.alwaysApply === true) {
      alwaysApplyRules.push(rule);
      continue;
  }
  if (rule.description) {
      rulebookRules.push(rule);
  }
  ```

  and the prompt builder injects the always-apply bucket full-text every turn
  (`forks/oh-my-pi/packages/coding-agent/src/system-prompt.ts:833` →
  `system-prompt.ts:851-852`, `rules: rules ?? [], alwaysApplyRules:
  injectedAlwaysApplyRules`). Both buckets survive the Manager's block-0
  REPLACE by construction (MP-1/DL-129 — the `custom-system-prompt.md`
  template still renders skills, always-apply rules, and the rulebook list).

- **The Implementer inherits the same skills and rules** — no second bundle:
  the `task` executor forwards the parent session's sets into every subagent
  ("Parent-discovered rules, forwarded to skip rule discovery in the
  subagent", `forks/oh-my-pi/packages/coding-agent/src/task/executor.ts:382-383`;
  forwarded at `task/structured-subagent.ts:422,426` — `skills,` / `rules:
  session.rules,` — into `task/executor.ts:2790,2793`). The implementer
  block-0 itself rides the mounted `agents/` dir
  (`ensureAgentDirLink(home, "agents", mounted.agentsDir)`, `cli.ts:570`).

**Consequence (mechanism fact, not a new decision):** at Dogfood the rule set
is **fleet-flat** — one `config/rules/` dir, every rule visible to both roles,
with the role split expressed as *tier* (always-apply = the Manager-facing
invariants, which also inject into implementer subagents; rulebook/domain =
loaded only when the description matches the work). Per-role bundle keying is
the named SEA-1724 seam (DL-078), not this record's scope. See OQ-1.

### Decision BI-2 — three sourcing modes, one per manifest row

Every battery is one of:

- **(a) Folded** — its content is absorbed into a role prompt or an existing
  SEA-1732 task; no standalone artifact ships. Two folds are Matt-ratified; a
  third (`enumerate-pr-review-surfaces`) is added by this amendment to close a
  manifest gap the critique surfaced (it was unaccounted-for in
  shipped/folded/excluded):
  - `delegated-implementation` → SEA-1732 **T5 management-trees** (DL-141).
    The stance is already in the Manager block-0 ("You are a COORDINATOR, not
    a typist … you never hand-write code"); the skill's unique value — the
    when-to-delegate litmus ("Delegate to an `implement` subagent when: the
    slice is **specified** … the work **fans out** … it's **mechanical
    volume**", wave `~/.agents/skills/delegated-implementation/SKILL.md:29-37`),
    the review-EVERY-diff discipline, and the brief contract — folds into
    T5's delegation-mechanics section. Its tier model is OMP-harness-specific
    and is stripped: the wave description names "`implement` subagents (Opus
    at medium thinking) … `implement-hard` (Opus at high thinking)"
    (`SKILL.md:3`) — wrong vocabulary for Compass, where T5 speaks in terms
    of the `task` mechanism and standing subagent defs (MP-5).
  - `version-control` (rule) → ONE Compass always-apply rule (DL-142). The
    wave rule is jj model + three footguns riding jj-vine/jj-hp wave infra —
    `~/.agents/rules/version-control.md:14-19`:

    ```text
    - **Auto-amend** — editing while `@` is a bookmark commit silently amends it. Run
      `jj new <bookmark>` first, then edit.
    - **Pushing** — `jj-vine submit <bookmark>` is the only push path (it self-gates
      through `jj-hp`). Never `git push`, and never open PRs with `gh pr create`.
    - **Review fixes are additive** — a new commit atop the bookmark, never
      amend + force-push (a force-push shows no interdiff on the PR).
    ```

    The Compass rule keeps the jj model + auto-amend + additive-review-fixes +
    never-`git push`, and its push path IS `jj-vine submit` — Compass KEEPS
    jj-vine as its stacked-PR tool (agents manage PR stacks with it: submit,
    the PR tree written into the PR description, and the forge's native
    stacked-diff integration via `gh stack`), retargeted to the Compass repo;
    the wave's `jj-hp` push-guard is Compass's own push-authorization concern,
    left to T8. No separate `stacking` skill either: the one adapted `jj`
    skill carries stacking (the wave `jj` skill already folds stacking into
    its references, "Deeper jj-vine mechanics/stacking and recovery live in
    references/", `~/.agents/skills/jj/SKILL.md:3`), and T8 adapts it directly.
  - `enumerate-pr-review-surfaces` (rule) → SEA-1732 **T9 review** skill; no
    standalone artifact. The wave rule is the review-status-reporting
    discipline — enumerate every PR feedback surface (inline threads, review
    summary bodies, top-level comments, CI, our own review findings) with
    zero-count evidence before reporting status — and its tooling pointer IS
    `github-pr-review` ("read `skill://github-pr-review`",
    `~/.agents/rules/enumerate-pr-review-surfaces.md:29`), which already folds
    into T9's `review` (DL-143). So it folds to the same home: the
    surface-enumeration discipline lands in T9's review loop. Its one
    CI-specific clause — the two-bucket false-green guard, that a check can
    live in the commit-STATUS bucket OR the check-RUNS bucket and a failure in
    one is invisible in the other (`enumerate-pr-review-surfaces.md:12`) — is
    cross-referenced by B4, whose GitHub-Actions retarget must preserve
    exactly that distinction. Not a placement fork: the record's own logic
    (github-pr-review → T9, and this rule pointing at github-pr-review) fixes
    T9 as home, with the CI-bucket guard surfacing in B4.
- **(b) Adapted-and-shipped** — the wave artifact ships under the same name
  with its invariant kept and its mechanics re-grounded in Compass (the GC-7
  discipline SEA-1732 froze). The adapt-not-copy passes beyond T3/T8/T9's
  already-planned adaptations:
  - `decision-authority` — the wave rule routes design forks to
    **Matt-via-`ask`, never the supervisor** ("Design/scope/approach forks
    are Matt's, asked directly via the `ask` tool — never routed through the
    supervisor", `~/.agents/rules/decision-authority.md:2`). Compass has no
    Matt and no `ask` tool; the adapted rule routes design authority to the
    **operator** (asked async on the home channel) and coordination to the
    **parent Manager** — same two-lane split, Compass parties.
  - `commit-conventions` — the wave rule commits "as seal with Matt as a
    co-author trailer; push your own feature branches via the seal-bot token
    — never main, never merge, allowlisted owners only"
    (`~/.agents/rules/commit-conventions.md:2`). The adapted rule keeps the
    Conventional-Commits subject + body-is-the-PR discipline and swaps the
    identity/push mechanics for Compass identity (the server-stamped
    attribution model, DL-050/DL-094) and Compass's submit path.
  - `hold-your-lane` — ships near-verbatim as a Manager always-apply rule
    ("A gated PR is not done — done means merged (or closed/dropped). Hold
    your lane until then", `~/.agents/rules/hold-your-lane.md:2`); mechanics
    references (review findings, CI, the merge gate) re-grounded in the
    Compass review loop (T9) and the human-merges gate.
  - `ci-failure-triage` — the discipline is universal, the hooks are not:
    step 1 pulls logs via "`skill://woodpecker-ci`: decode the check's
    `details_url` → `pipeline ps` → `log show <step>`"
    (`~/.agents/skills/ci-failure-triage/SKILL.md:18-19`). The adapted skill
    keeps the 4-step classification (read the real log; bucket
    yours-vs-not-yours; classify code-vs-env/permission; verify in a
    reproducing env) as the CI-engine-NEUTRAL default; the GitHub-Actions
    tooling hooks (log-pull, check-decoding) leave the default for the
    SEA-1739 CI pack seam, same shape as a language pack (DL-147). Moderate
    revision, not verbatim.
  - The implementer domain rules `pre-finish-checks`, `no-retries`,
    `process-safety`, `planning-evidence` ship with invariants intact (each
    is already tool-agnostic; see the Plan table for the light touch each
    needs).
- **(c) Authored new** — no wave source exists:
  - `devenv` — provision + enter + warm a per-repo devenv/direnv shell, run
    tools through it. A real Dogfood gap: the wave analog is the checkout's
    own AGENTS.md toolchain stanza ("The toolchain is proto … plus devenv …
    Enter the dev shell with `direnv allow`, then `bun install`", compass
    `AGENTS.md:9-11` at `cf048ca`), which a spawn only benefits from if it
    knows to look. Pairs with T6 compass-setup.
  - `issue-lifecycle` — own an issue end-to-end (take, drive state, close)
    plus the PR review loop on the Compass surface. **Gated on SEA-1734's
    issue/PR tools** (the same gate the Manager block-0's issue-ownership
    lines carry as `[TODO SEA-1734]`, MP-4): the skill names concrete tools,
    so it cannot ship before they land.

### Decision BI-3 — language-neutral default; packs are Beta (DL-140)

The shipped default bundle contains **no language-specific content** — none of
the wave's `go-*` rules or `golang-*` skills. Ratified rationale: for Dogfood,
Matt-as-user adds the Go skills he wants via the normal user-skill path (they
are his to add, not a Compass default); Beta ships the "packs" feature
(**SEA-1739**: per-repo language → skill-pack selection) so the Go skills
become easily usable by other users. The Beta boundary is a named seam, not a
gap: nothing in this manifest needs reopening when packs land — a pack is
additive bundle content on the same delivery surface (BI-1).

**Checkable form of the neutrality gate (DL-147).** The gate is
stack-neutral, not merely language-neutral: no default battery may name a
language- OR stack-specific command as NORMATIVE (illustrative tables are
permitted, marked as such). Two consequences fix the edges the critique
surfaced. (1) `devenv`/`direnv` is a deliberate PRODUCT BASELINE, not pack
territory — "devenv is baked into the product" (Matt, 2026-08-05); B3 ships
it as a default skill. (2) CI-engine specifics (GitHub-Actions log-pull,
check-decoding) ARE pack territory — the default `ci-failure-triage` skill
ships CI-engine-neutral (the 4-step classification discipline) and the
GitHub-Actions hooks become an SEA-1739-shaped pack seam, same shape as a
language pack. See BC-4 and B4.

### Decision BI-4 — the exclusion set (DL-143)

Excluded outright as wave-specific infrastructure that would confuse a Compass
agent (each with the one-line reason; the full table is in the Plan):
`multi-agent-wave`, `spawn-agent`, `wave-status-sync`, `session-recovery`,
`nix-hosts`, `github-pr-review`, `woodpecker-ci`, `zellij-session-safety`.
Exclusion is deliberate curation, not deferral. Six of the eight are TRUE
exclusions — their content ships nowhere in the batteries — and DL-143's
no-revival posture is scoped to those six: any future need for one routes
through a new decision, not a revival. The remaining two (`github-pr-review`,
`woodpecker-ci`) are excluded only as standalone ARTIFACTS; their content
FOLDS in (github-pr-review → the adapted `review`, T9; woodpecker-ci's triage
discipline → the CI-engine-neutral `ci-failure-triage`, B4), so they are not
what the no-revival posture governs.

## Global Constraints

Every task below inherits these (extending SEA-1732's GC set, which T-level
work under this record also inherits):

- **BC-1 — Manifest-level record.** This record fixes names, sources, modes,
  tiers, and gates. Skill/rule BODIES are authored in the impl children; no
  body text is frozen here.
- **BC-2 — Adapt, don't fork** (SEA-1732 GC-7): keep the invariant, re-ground
  the mechanics in Compass tools. Wave artifact names are kept unless the
  content's scope changed (see OQ-2).
- **BC-3 — Name only what exists** (SEA-1732 GC-3/MP-4): a skill/rule line may
  name a tool only if it exists at the commit it ships against; unshipped
  affordances are explicit `[TODO <issue>]` lines. This is what gates
  `issue-lifecycle` on SEA-1734.
- **BC-4 — Language-neutral** (BI-3): no `go-*`/`golang-*` content in any
  default battery; a task that finds itself needing one has hit the SEA-1739
  boundary and stops.
- **BC-5 — Tier by frontmatter.** Always-apply rules carry
  `alwaysApply: true`; domain rules carry `description:` (and globs/conditions
  where useful) — the split is enforced by `bucketRules` (BI-1 evidence), so
  the frontmatter IS the tier decision and each task's table row fixes it.
- **BC-6 — Always-apply stays terse.** Full text every turn on every session
  (including forwarded into subagents, BI-1) — each always-apply rule is
  screens-of-one, not screens-of-three.
- **BC-7 — Forwarded always-apply rules self-scope by role.** The `task`
  executor forwards the parent session's rules into every subagent (`rules:
  options.rules`, `task/executor.ts:2793`) but NOT the session `customTools`
  (`customTools: mcpProxyTools.length > 0 ? mcpProxyTools : undefined`,
  `task/executor.ts:2829` — MCP-proxy tools only), so a Manager always-apply
  rule rides every implementer-subagent turn with no access to the
  Manager-only affordances it assumes (comms/lifecycle natives, the issue/PR
  surface). Each forwarded always-apply rule's body MUST therefore be correct
  for BOTH roles: branch explicitly where behavior differs — `hold-your-lane`,
  `decision-authority`, and the T3-owned `own-your-issue` each state the
  hands-subagent behavior AND the Manager behavior, e.g. "If you are a hands
  subagent: execute the briefed slice, report, and yield — do not hold the
  lane or drive issue state. If you are the Manager: the full invariant." A
  role-invariant rule (`never-block`) needs no branch: its
  single behavior already applies to both. This is body-level self-scoping,
  the authored-body contract B2/B7 (and the cross-referenced T3 amendment to
  `own-your-issue`) implement — NOT per-role delivery, which is the SEA-1724
  seam (OQ-1). (DL-144.)

## Plan

The manifest, per role. "Source" names the wave artifact (or the SEA-1732
task); "Mode" is BI-2's (a) fold / (b) adapt / (c) new, plus "T3/T4…" where
SEA-1732 already owns the row and this record only confirms it in the bundle.

### Manager — always-apply rules (`config/rules/*.md`, `alwaysApply: true`)

| Rule | Source | Mode | Adaptation note |
| --- | --- | --- | --- |
| `never-block` | wave rule | adapt (SEA-1732 T3) | re-ground in comms tools + turn-yield loop |
| `own-your-issue` | wave rule | adapt (SEA-1732 T3) | re-ground in Compass issue surface (`[TODO SEA-1734]` on concrete tools); **role-aware body (BC-7)** — issue-state ownership is Manager-only; a hands subagent neither drives nor closes issues (cross-ref amendment to T3's brief) |
| `red-green-testing` | wave rule | adapt (SEA-1732 T3) | invariant unchanged; examples re-grounded |
| `never-merge` | new (SEA-1732 T3) | new one-liner | the human merges — already frozen in T3's set |
| `design-first` | new (SEA-1732 T3) | new one-liner | already frozen in T3's set |
| `compact-often` | new (SEA-1732 T3) | new one-liner | already frozen in T3's set |
| `hold-your-lane` | wave rule | adapt (**this record**, B2) | merged-or-closed is done; re-ground review/CI/merge-gate references in T9's loop + the human merge gate; **role-aware body (BC-7)** — a hands subagent executes its slice and yields, it does not hold a lane |
| `version-control` | wave rule (3 footguns) | fold (**this record**, B1 — DL-142) | keep jj model + auto-amend + additive-fixes + never-`git push`; keep jj-vine as the stacked-PR tool, push path = `jj-vine submit` retargeted to the Compass repo (push-guard mechanism T8's); **B1 ships with or after T8, never before (DL-145)** — no `[TODO T8]` placeholder in an always-apply rule |
| `decision-authority` | wave rule | adapt (**this record**, B7) | Matt/`ask`/supervisor → operator (async, home channel) / parent Manager; **role-aware body (BC-7)** — a hands subagent cannot reach the home channel (comms tools are session `customTools`, not forwarded, executor.ts:2829), so its branch escalates forks to the parent Manager |

### Manager — skills (`config/skills/<name>/SKILL.md`)

| Skill | Source | Mode | Gated on |
| --- | --- | --- | --- |
| `comms-playbook` | authored | SEA-1732 T4 | — (`[TODO SEA-1722]` lines inside) |
| `management-trees` | authored **+ delegated-implementation fold** | SEA-1732 T5 + fold (B6 — DL-141) | — |
| `compass-setup` | authored | SEA-1732 T6 | — |
| `supervisor-channel` | authored | SEA-1732 T7 | — |
| `manager-coordination-channel` | authored | SEA-1732 T7 | — |
| `jj` | wave `jj` skill | adapt, SEA-1732 T8 | — |
| `review` | wave `review` skill (+ `github-pr-review` **and `enumerate-pr-review-surfaces`** folded in) | adapt, SEA-1732 T9 | — |
| `design` | wave `design` skill | adapt, SEA-1732 T9 | — |
| `devenv` | none (new) | new (**this record**, B3) | — |
| `ci-failure-triage` | wave skill | adapt (**this record**, B4) | ships **CI-engine-neutral** (the 4-step classification discipline); GitHub-Actions-specific hooks deferred to the SEA-1739 pack seam (DL-147) |
| `issue-lifecycle` | none (new) | new (**this record**, B5) | **SEA-1734** issue/PR tools |

### Implementer — domain rules (`config/rules/*.md`, rulebook tier)

Delivered in the same flat `config/rules/` dir; rulebook tier (description
match, body on demand) so they cost the Manager nothing beyond a one-liner.
The implementer inherits them via the task-tool rules forwarding (BI-1).

| Rule | Source | Mode | Adaptation note |
| --- | --- | --- | --- |
| `pre-finish-checks` | wave rule | adapt (B8) | "format + lint + tests for the affected area" — re-ground gate commands in the devenv shell (B3) |
| `no-retries` | wave rule | adapt (B8) | invariant + TTSR conditions carry as-is (tool-agnostic) |
| `process-safety` | wave rule | adapt (B8) | **rulebook tier (DL-146)** — per-container session isolation (one container per standing agent, DL-143) removes the cross-agent/host clobber blast radius that made the wave file `alwaysApply: true`; the intra-container sibling-process residual (a Manager and its in-process `task` subagents share one container) is covered by rulebook forwarding (BC-7) + on-demand pull on a destructive kill, not always-on injection; see OQ-3 |
| `planning-evidence` | wave rule | adapt (B8) | invariant unchanged (file+line + quoted snippet) |
| `commit-conventions` | wave rule | adapt (**this record**, B7) | seal-bot identity/allowlist/jj-vine push → Compass attribution model (DL-050/DL-094) + Compass submit path; keep Conventional Commits + body-is-the-PR |

The implementer needs no skill rows of its own: it inherits the session skill
set (BI-1), and the skills above are deliberately role-neutral in body
(`jj`, `ci-failure-triage`, `devenv` serve both roles).

### Exclusions (DL-143)

| Wave artifact | Why excluded |
| --- | --- |
| `multi-agent-wave` (skill) | The wave's fleet model (zellij panes, tracker, overnight mode) — Compass's tree/channel model replaces it (T5/T7) |
| `spawn-agent` (skill) | OMP/zellij/cotal spawn mechanics — Compass spawning is `agents_spawn_peer` + subagents (MP-5) |
| `wave-status-sync` (skill) | Tracker/Linear reconciliation for the wave — Compass state is canonical server-side (DL-032/DL-070) |
| `session-recovery` (skill) | OMP-session JSONL surgery on the wave box — Compass session persistence is server-owned |
| `nix-hosts` (skill) | Matt's personal nix-config flake — not a product concern |
| `github-pr-review` (skill) | Excluded as a standalone ARTIFACT; its content FOLDS into the adapted `review` (T9) — one review surface, not two skills (a fold, not one of the six true exclusions DL-143's no-revival posture governs) |
| `woodpecker-ci` (skill) | Excluded as a standalone ARTIFACT; its content FOLDS into `ci-failure-triage` (B4) — the CI-engine-neutral triage discipline survives; Woodpecker/GitHub-Actions specifics are pack territory (DL-147) (a fold, not one of the six true exclusions) |
| `zellij-session-safety` (rule) | Compass agents are session-isolated (one container per standing agent) — the shared-multiplexer hazard does not exist |

### Gates and boundaries

- **SEA-1734** — the issue/PR tools (pre-Dogfood, operator-provisioned
  surface). Gates B5 (`issue-lifecycle`) entirely, and the `[TODO SEA-1734]`
  lines inside `own-your-issue`. B5 ships in the same PR that lands the tools
  or later, never before (BC-3).
- **SEA-1739** — the language-pack mechanism (Beta). Boundary only: nothing in
  this record blocks on it, and no task may smuggle language content past it
  (BC-4).
- **PR #1089** — the SEA-1732 record. The T3/T4–T9 rows above are owned there;
  this record's tasks (B1–B8) are strictly additive to that set and B6 is an
  amendment to T5's brief, cross-referenced, not a rewrite.

## Tasks

Impl children that author the skill/rule bodies (mirroring SEA-1732's T4–T9
granularity: one artifact or one tight pair per review cycle). All are
prose-authoring tasks; none touches entrypoint code.

- [ ] **B1 — `version-control` always-apply rule (the footguns fold, DL-142).**
  Interfaces: consumes wave `~/.agents/rules/version-control.md` + T8's
  `jj` skill (for the Compass push recipe) → produces
  `config/rules/version-control.md` (`alwaysApply: true`). **Hard-ordered
  AFTER T8 (DL-145): B1 ships with or after T8, never before.** This is an
  always-apply rule injected full-text every turn (BC-6) and its whole point
  is naming the one correct push path, so it cannot ship the `never git push`
  invariant with only a `[TODO T8]` placeholder where the submit verb goes —
  the `[TODO T8]` escape hatch is struck. Same BC-3 posture B5 takes with
  SEA-1734 ("ships with or after the tools, never before"): T8 names the
  Compass submit path first, B1 then writes it in.
- [ ] **B2 — `hold-your-lane` always-apply rule.** Interfaces: consumes wave
  `~/.agents/rules/hold-your-lane.md` + T9's review-loop vocabulary →
  produces `config/rules/hold-your-lane.md` (`alwaysApply: true`).
  **Role-aware body (BC-7):** the rule forwards into every implementer
  subagent (executor.ts:2793), where "done means merged, hold your lane,
  don't pick up new work" contradicts the hands contract (execute one briefed
  slice, report, then yield — SEA-1732 T2). The body MUST branch: "If you are
  a hands subagent: finish the briefed slice, report, and yield — you hold no
  lane. If you are the Manager: <the merged-or-closed invariant, re-grounded
  in T9's loop + the human merge gate>."
- [ ] **B3 — `devenv` skill (new).** Provision + enter + warm a per-repo
  devenv/direnv shell; run all gates/tools through it. Interfaces: consumes
  the compass `AGENTS.md:9-11` toolchain stanza (the shape to generalize) +
  T6 compass-setup (which references it for workspace stand-up) → produces
  `config/skills/devenv/SKILL.md`.
- [ ] **B4 — `ci-failure-triage` skill (adapted, CI-engine-NEUTRAL — DL-147).**
  Keep the CI-engine-neutral triage discipline as the default skill body: the
  4-step classification (read the real log; bucket yours-vs-not-yours;
  classify code-vs-env/permission; verify in a reproducing env), and the
  two-bucket false-green guard as a general principle. Do NOT hard-code
  GitHub-Actions log-pull or check-decoding as the default — those
  GHA-specific hooks are named as the Dogfood grounding that lives in the
  SEA-1739 CI pack, not the default battery (same seam shape as a language
  pack, BC-4). Interfaces: consumes wave
  `~/.agents/skills/ci-failure-triage/SKILL.md` → produces
  `config/skills/ci-failure-triage/SKILL.md`. **Cross-ref (BI-2(a) fold — do
  not drop):** carry `enumerate-pr-review-surfaces`'s two-bucket false-green
  guard as a general principle: a check's result can surface on more than one
  independent reporting channel, and a red on one is invisible if you read
  only another, so triage MUST read every surface on the exact head SHA
  before reporting green (the GitHub-API commit-status-vs-check-runs split
  that concretizes this on the Compass forge is SEA-1739 pack detail).
- [ ] **B5 — `issue-lifecycle` skill (new, GATED SEA-1734).** Own an issue
  end-to-end + the PR review loop on the Compass surface, naming the concrete
  SEA-1734 tools. Interfaces: consumes the SEA-1734 tool surface (as landed) +
  the Manager block-0 work-loop lines → produces
  `config/skills/issue-lifecycle/SKILL.md`. Ships with or after the tools,
  never before (BC-3).
- [ ] **B6 — T5 management-trees fold (DL-141, cross-ref SEA-1732 T5).** Fold
  the when-to-delegate litmus, review-every-diff discipline, and brief
  contract into T5's delegation-mechanics section; strip OMP tier naming
  (`implement`/`implement-hard`/thinking levels → the `task` mechanism +
  standing subagent defs, MP-5). Interfaces: consumes wave
  `~/.agents/skills/delegated-implementation/SKILL.md` + T5's brief →
  produces the delegation section of `config/skills/management-trees/SKILL.md`
  (T5's artifact — this task amends T5's brief, it does not create a file).
- [ ] **B7 — adapt-not-copy pass: `decision-authority` + `commit-conventions`.**
  Interfaces: consumes wave `~/.agents/rules/{decision-authority,commit-conventions}.md` +
  the Compass attribution decisions (DL-050/DL-094) → produces
  `config/rules/decision-authority.md` (`alwaysApply: true`) and
  `config/rules/commit-conventions.md` (rulebook tier). **`decision-authority`
  is role-aware (BC-7):** it is always-apply, so it forwards into every
  implementer subagent (executor.ts:2793), but that subagent cannot reach the
  home channel it routes forks to — the comms native tools are session
  `customTools`, which the executor does NOT forward (executor.ts:2829,
  MCP-proxy tools only). The body MUST branch: "If you are a hands subagent:
  you cannot reach the operator; escalate a design fork to your parent Manager
  and keep to your briefed slice. If you are the Manager: <design forks to the
  operator, async on the home channel; coordination to the parent>."
- [ ] **B8 — implementer domain-rule pass:** `pre-finish-checks`, `no-retries`,
  `process-safety`, `planning-evidence`. Interfaces: consumes the four wave
  rule files + B3's devenv vocabulary (for pre-finish-checks' gate commands)
  → produces `config/rules/{pre-finish-checks,no-retries,process-safety,planning-evidence}.md`
  (all rulebook tier per the Plan table — `process-safety` included, at
  rulebook not always-apply (DL-146)). **`process-safety` intra-container
  residual:** excluding
  `zellij-session-safety` (DL-143) drops the shared-multiplexer hazard but NOT
  the sibling-process one — a Manager and its in-process `task` subagents share
  one container, so a broad or wrong-PID kill still takes down a sibling's
  work. `process-safety`'s explicit-PID-only discipline (kill only a PID you
  started this turn, else ask — `~/.agents/rules/process-safety.md:10-14`) is
  what carries that guard; B8 keeps it intact for exactly this reason.

## Open Questions

Batched for Matt (this record has no ask path; the coordinator relays after
critique). Each marked load-bearing or non-load-bearing, with a
recommendation; the record is designed against the recommendation.

1. **Fleet-flat rules: Manager always-apply rules also inject into
   implementer subagents — RESOLVED (Matt, 2026-08-05, role-aware ruling;
   DL-144).** Mechanism fact, re-verified firsthand at `cf048ca`: the `task`
   executor forwards the parent session's rules into every subagent (`rules:
   options.rules`, `task/executor.ts:2793`) but does NOT forward the session
   `customTools` — only the MCP-proxy tools cross the seam (`customTools:
   mcpProxyTools.length > 0 ? mcpProxyTools : undefined`,
   `task/executor.ts:2829`). So every Manager always-apply rule rides every
   implementer-subagent turn, and the earlier "harmless" reading was FALSE:
   two of these rules are not merely a token cost, they are actively WRONG on
   a subagent. `hold-your-lane` ("done means merged … hold your lane … don't
   pick up new work") contradicts the implementer's frozen hands contract
   (execute one briefed slice, report, then yield — SEA-1732 T2);
   `decision-authority` routes a design fork to the operator "on the home
   channel" — a channel the subagent mechanically CANNOT reach, because the
   comms native tools are session `customTools` and those are exactly what
   `:2829` does not forward; and `own-your-issue` commands driving issue state
   that belongs only to the Manager. **Resolution (a ruling, not a deferred
   fork): every forwarded always-apply rule is authored ROLE-AWARE (BC-7) — its
   body is correct for both roles, branching explicitly where behavior differs,
   so an implementer reads the branch written for it. Per-role rule *bucketing*
   (delivering different rule sets per role) remains the SEA-1724 seam and is
   not built early; role-awareness lives in the authored body instead.**
2. **Name of the footguns rule: keep `version-control` vs rename
   `version-control-footguns` — RESOLVED (Matt, 2026-08-05):** keep
   `version-control` (confirmed). Same discovery name as the wave, and the
   rule still IS the version-control invariant set, only re-grounded; the fold
   is into ONE Compass always-apply rule under that name.
3. **`process-safety` tier — RESOLVED (Matt, 2026-08-05; DL-146):** ships at
   **rulebook tier**, not always-apply. The wave file is `alwaysApply: true`
   (`~/.agents/rules/process-safety.md:3`) because on the shared wave box a
   broad or wrong-PID kill could clobber other agents or Matt's running
   processes — the unrecoverable-failure argument for always-apply was really
   about that shared-box blast radius. Per-container session isolation (one
   container per standing agent, DL-143) removes it, so the always-apply cost
   on every Manager turn is no longer justified; rulebook tier is correct. The
   intra-container sibling-process residual is covered by rulebook forwarding
   (BC-7) + the on-demand pull before a destructive kill, not always-on
   injection. The record now reflects rulebook tier (Plan table, B8, BC-7).
4. **Config-bundle authoring skill (`compass config put`, materialize,
   Reload) — RESOLVED (Matt, 2026-08-05):** NOT in the default batteries
   (confirmed). It is operator tooling today; revisit via a new decision row
   (supersession, per DL-143's no-revival posture applying only to the
   excluded wave set) if/when agents ever author bundles.
5. **DL numbering — RESOLVED (Matt, 2026-08-05):** a mechanical merge-time
   reconcile (confirmed). This record assumes #1089 lands DL-129..DL-134 first
   and takes DL-140..DL-147; if another record lands rows in between, the
   coordinator renumbers at merge (the ledger is append-only by ID, so this is
   a mechanical shift, not a content fork). Non-load-bearing.
