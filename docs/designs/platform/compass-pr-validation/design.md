# Compass PR validation — agent-self-tested coverage, evidence on the PR

> **Design record.** How a Compass PR gets validated without a human pulling
> the branch: an expanded agent-self-run e2e harness for behavioral changes,
> results surfaced ON the PR, a previewable UI build, and a validation skill
> bundled with the Compass agents — so review time is judgment, not
> reproduction. Extends the existing e2e harness record
> (`docs/designs/platform/compass-dogfood-e2e/design.md`, SEA-1681, H1-H8).
> Local-dev / gate mechanics are a separate record
> (`docs/designs/platform/compass-local-dev/design.md`). UI-visible changes
> are additionally previewable in an isolated PR-preview environment defined
> in the private infrastructure design repo; that environment's hosting,
> deploy workflow, and selection mechanism live in that repo's records, not
> here — this record owns only the repo-side validation surfaces, including
> the configurable UI build the preview consumes.

Status: **Draft**

## Problem / Intent

Agents submit Compass PRs; the human review cycle is the bottleneck. A
behavioral change today forces the reviewer to trust the diff; a user-visible
change forces a local build-and-click. The intent: maximize what agents
end-to-end test THEMSELVES, and give the reviewer a one-click review surface —
a pass/fail + artifacts summary on the PR for behavioral changes, and captured
visual evidence (screenshots / recordings) for user-visible ones — so review
is judgment, not reproduction.

## Approach

One validation lane plus the two decided folds that make it get used:

```text
  PR opened ──► expanded e2e harness (deterministic canned tier, agent-authored
                scenarios) → per-PR CI gate + summary + artifacts ON the PR
             ─► path-filtered live-model e2e leg (GitHub Actions; runs only
                when model/agent-relevant paths change)          [DECIDED]
  either way ─► the bundled PR-validation skill: every Compass agent carries
                it by default; validation evidence (described click-path now;
                screenshots/recordings once the B6 capture stack ships)
                before review-ready                              [DECIDED]
```

### A2 — the expanded e2e harness, agent-self-run

**Base.** The harness exists and is merged: `go/e2e` is the podman-tagged
package over `stack.Up` (fixture primitives in `go/e2e/agent_ops.go:16-129`:
`CreateAgent`/`Provision`/`StartSession`/`Resume`/`AwaitSessionSettled`/
`RemoveWorkspace`), with the deterministic canned-model backend
(`WithCannedModel`/`WithCannedScript`, `go/e2e/fixture.go:76-103`) and the
cross-restart teardown/idempotence leg (`go/e2e/legsix_test.go:14-31`). The
per-PR CI gate is H8's lane, wired by PR #256 ("adds the `Dogfood e2e
(deterministic full-stack tier)` step to `ci.yml` … then `go test -tags podman
./e2e/...` under `-race`", pr://256 body; stacked on #303's `replay_complete`
emission, must-not-merge-before). This record EXTENDS that harness — new
scenarios compose the existing primitives (the harness record's own contract:
"New scenarios … compose the same primitives — the scenario set grows without
touching the core", `docs/designs/platform/compass-dogfood-e2e/design.md:379-381`).

**Target scenario set** (each = one scenario file in `go/e2e`, composing
fixture primitives; coverage state grounded):

| Scenario | State today | What the expansion adds |
| --- | --- | --- |
| Multi-agent communication | Legs 3+4 merged: agent-driven spawn + `@mention` steer/deliver split (harness record H4, `design.md:634-665`) | Multi-peer fan-out: 3+ agents, channel policy (OWNER_ONLY, `proto/compass/v1/comms.proto:239-241,252-257`), unmentioned-subscriber deliver at scale |
| PR submitting | Wire contract exists on paper only — `AgentGateway.Forge` carries the envelopes (`proto/compass/v1/agent_gateway.proto:79-90`) and the relay procedure is generated (`RunnerServiceRelayForgeCallProcedure`, `go/internal/gen/compass/v1/compassv1internalconnect/runner.connect.go:74-77`) — but FOUR execution layers are missing: no agent-side forge tool (`packages/compass-agent/src/` has only the `comms.ts`/`lifecycle.ts` brokers); no Runner `Forge` handler; no Server relay leg (`go/internal/runnerhub/` has `relay_comms.go`/`relay_lifecycle.go`/`relay_board.go`, no `relay_forge.go`; the Handler embeds `UnimplementedRunnerServiceHandler`, `handler.go:44-45`, so `RelayForgeCall` returns `CodeUnimplemented` today, `go/internal/gen/compass/v1/compassv1internalconnect/runner.connect.go:609-611`); no server-side forge write executor (`go/server/board.go:73-74,108-109`) | Split out as a stub handed to the forge execution stack's lane (DL-052) as THAT lane's acceptance test — see B3 |
| Provision/enroll | Leg 1 + leg 6 merged: bring-up through enrollment, teardown idempotence across a stack restart (`go/e2e/legsix_test.go:14-31`) | Re-enroll after runner restart with live sessions; provision under a revoked/expired runner token (fail-closed assert) |
| Home-channel first-turn | Re-modeled by PR #256: "legs 2 / 3-4 / 5 now drive the first turn by posting to the agent's home channel (the post-`initial_prompt` contract), delivered via the live fan-out" (pr://256 body). `initial_prompt` is already removed on this checkout — `reserved` at `compass.proto:590` and `:613` (see `compass-initial-prompt-removal.md`) — so the home-channel-first-turn delivery contract is the CURRENT state, not a pending re-model; the home channel is minted at CreateAgent (`proto/compass/v1/comms.proto:174-175`) | First-class scenario: post-to-home-channel → delivery → first turn settles → reply lands back in the home channel; #256's delivery-cursor event-gate promoted to a shared primitive |
| Replay | Replay-then-Live subscribe waits exist (`SubscribeComms`, harness record A2); `replay_complete` is the agent control-lane barrier ("on receipt the Runner releases the live ops held behind the restart replay barrier", `proto/compass/v1/agent.proto:64-67`), fresh-start emission lands with #303 | A restart-replay scenario: kill/restart the runner mid-conversation, assert the replay barrier holds live ops until `replay_complete_ack`, no duplicate or lost delivery across the barrier |

**How agents self-run them.** Two surfaces, both existing-mechanism:

1. **CI (deterministic, gating).** #256's `ci.yml` step runs the full suite on
   every PR — the floor every PR gets with zero agent effort, riding the
   affected-detection gate (`moon ci :ci` on PRs,
   `.github/workflows/ci.yml:263-264`) plus the dedicated e2e step.
2. **Pre-submit, in the agent's own workspace.** The submitting agent runs the
   suite itself against its branch before opening the PR: `go test -tags
   podman ./e2e/...` — the fixture compiles the child binaries from the tree
   and stands up a private, ephemeral stack (`go/e2e/fixture.go:132-145`), so
   nothing touches any shared environment. The bundled validation skill (A3)
   makes this a submit-gate the agent owns, not an optional extra.

**How results surface ON the PR.** Three surfaces, one task (B5):

- the required check itself (#256's step) — pass/fail;
- `$GITHUB_STEP_SUMMARY` — a per-scenario table (name, verdict, duration)
  written by the e2e step, visible on the check page without opening logs;
- artifacts — on failure, the step uploads the stack's state-dir logs +
  per-scenario transcripts via `actions/upload-artifact`, and one sticky PR
  comment links the run + artifacts (and the PR-preview link, when the
  isolated PR-preview environment has published one), so the reviewer reads
  verdict → summary → artifacts without pulling code.

### A3 — the bundled PR-validation skill (DECIDED)

**Ruled by Matt:** the PR-validation guidance is a **default validation skill
bundled with the Compass agents** — Compass ships/provisions it to its agents
as part of their standard equipment, NOT via personal dotfiles and not merely
an `AGENTS.md` prose rule. Every agent working in Compass carries it without
per-user or per-host setup. (An earlier draft framed this as a rule + skill
authored in a personal dotfiles repo and delivered by an external config
spine; that framing is replaced by this ruling.)

The skill's requirement: agents working in Compass **always capture
screenshots / screen-recordings where applicable** as validation evidence.
Concretely, it is PHASED, because the capture stack does not exist yet (B6):

- **Phase 1 (immediate):** a user-visible change ⇒ the PR body carries a
  described click-path (what to open, what to click, what changed) plus the
  PR-preview link when one is published; a behavioral change ⇒ name the
  covering e2e scenarios, or run the suite per A2.2 and state the result.
- **Phase 2 (activates when B6's capture stack ships):** a screenshot (static
  UI change) or screen recording (interaction/motion change), captured from
  the actually-running artifact — the PR preview or a locally-driven build —
  never a mockup.

Skill content: the evidence decision tree (UI-visible vs behavioral), the
click-path template (phase 1), capture + attach mechanics (phase 2, once
`tools/capture` exists), the e2e invocation (`go test -tags podman
./e2e/...`), and the validation section of the PR body. It composes with the
existing pre-finish checks discipline; it does not duplicate
format/lint/test.

### A4 — CI tiers: canned every-PR floor + path-filtered live-model leg (DECIDED)

**Ruled by Matt:** two tiers, both in GitHub Actions.

- **The deterministic canned tier is the cheap EVERY-PR floor and the required
  gate.** `WithCannedModel`/`WithCannedScript` (`go/e2e/fixture.go:76-103`)
  drive a stub SSE backend compiled into the harness — **no credentials of any
  kind**. Verified this session: a grep of `go/e2e` for
  LITELLM/ANTHROPIC/OPENAI/api-key credential material finds zero (the only
  bearer in the package is the fixture's own stack-local admin bearer,
  `go/e2e/clients.go:38-49`, and the one `anthropic/...` string is an
  illustrative model id the canned backend overrides,
  `go/e2e/fixture.go:204,215-218`). This tier is unaffected by the live leg
  and remains the required merge gate.
- **A SEPARATE live-model e2e leg runs in GitHub Actions, gated by a PATH
  FILTER**, so expensive LLM tests do NOT run on every PR — only when
  relevant paths change. Proposed trigger globs (B7 finalizes):
  `go/e2e/**`, `go/internal/runner/**`, `go/internal/runnerhub/**`,
  `packages/compass-agent/**`, `proto/compass/v1/agent*.proto`. The leg
  injects the LLM key from a GitHub Actions secret; it is never a required
  check, and its absence on an unrelated PR is by design, not a gap.

## Global Constraints

- **Canned tier = every-PR required floor.** The deterministic tier gates
  every PR with zero credentials; the live-model leg is additive,
  path-filtered, and never required (A4).
- **Event-gated determinism.** New e2e scenarios follow the harness's
  discipline — no sleeps, no retries; assertions wait on bus subscriptions,
  store reads, or session-frame streams (harness record Global Constraints,
  `compass-dogfood-e2e/design.md:81-88`; the merged `AwaitSessionSettled` is
  "FULLY EVENT-GATED", `go/e2e/agent_ops.go:84-91`).
- **Podman-tagged, skip-not-fail.** New scenarios live in `go/e2e` under
  `//go:build podman` with the `podmanUsable()` skip guard
  (`go/e2e/fixture.go:1,494-500`).
- **One-job CI doctrine.** New CI wiring extends steps inside the existing
  `CI` job or a least-privilege sibling workflow, never a project-enumerating
  matrix (`.github/workflows/ci.yml:4-18`); the live-model leg and the
  sticky-comment surface are separate minimal-permission workflows, off the
  required-check hot path.
- **Scripts over bash.** Any summary/capture logic beyond a one-liner is a
  bun/TS tool under `tools/`, not shell in YAML.
- **De-leak.** This is a public repo. The isolated PR-preview environment is
  referred to only generically — "an isolated PR-preview environment defined
  in the private infrastructure design repo". No internal hostnames,
  addresses, ports, service/unit names, or private repo names appear in this
  record or in anything it produces (workflow YAML, skill text, PR comments).
- **Sequencing floor.** Harness tasks that extend the CI gate land AFTER #256
  (the `[ci]` gate wiring) and #303 (`replay_complete` fresh-start emission)
  merge; the replay scenario (B4) additionally consumes #303's emission as
  its subject.
- Commits authored as seal with Matt's co-author trailer
  (rule://commit-conventions); this is a platform-corpus record, so the
  design-ledger-gate does not govern it (`tools/design-ledger-gate/index.ts:45`
  scopes `PRODUCT_DIR = "docs/designs/product"`; a platform path "is not a
  record", `index.test.ts:124-126`).

## Plan

Owner lanes: `[compass-ui]` = apps/ui; `[harness]` = go/e2e; `[platform]` =
CI wiring + agent equipment. (Task ids keep the source record's numbering for
traceability; A1 and B2 — the preview deploy workflow and its env topology —
live in the private infrastructure design repo's preview record, not here, so
the Approach opens at A2 and the Plan has no B2.)

### B1 [compass-ui] — previewable UI build: env injection for a configurable target

A general compass-ui capability: make the UI bundle buildable against ANY
configured door URL + bearer via build-time env — the isolated PR-preview
environment (defined in the private infrastructure design repo) consumes this
capability; local/dev targets can too. No code change to connection
resolution is expected — `resolveConnection` already requires a base URL and
normalizes an absent token to a no-auth client
(`apps/ui/src/live/connection.ts:45-58`). The build is parameterized by
`VITE_COMPASS_BASE_URL` (the target door URL) and `VITE_COMPASS_TOKEN` (a
bearer minted by the consuming environment). One deployed preview serves from
a single stable URL at the root path, so no per-PR Vite `base` wiring is
needed (today `apps/ui/vite.config.ts:7-11` sets no `base`, and none is
required at the root).

Interfaces:

- Consumes: `resolveConnection(env: CompassEnv): Connection` with
  `CompassEnv{VITE_COMPASS_BASE_URL?, VITE_COMPASS_TOKEN?}`
  (`apps/ui/src/live/connection.ts:30-33,45`); moon task `compass-ui:build` =
  `bunx vite build` → `dist` (`apps/ui/moon.yml:19-23`);
  `IssueTokenRequest{account_id} → IssueTokenResponse{token}`
  (`proto/compass/v1/compass.proto:663-675`) as the bearer-mint RPC the
  consuming environment calls (re-runnable per deploy — a disposable
  environment mints a disposable token).
- Produces: `moon run compass-ui:build` parameterized by
  `VITE_COMPASS_BASE_URL`/`VITE_COMPASS_TOKEN` yielding a `dist/` that boots
  against the configured network door.

Test cycle: red — no build input targets a configured TLS door today; green —
`bunx vite build` with the env set yields a `dist/` whose `bootConnection`
reaches WhoAmI against a TLS door (verified against the dev stack's own
network door, which devenv already binds: `devenv.nix:255-257`
`--listen … --tls-cert … --tls-key`).

### B3 [harness] — scenario expansion: multi-peer fan-out, provision/enroll hardening

Two new scenario files in `go/e2e`, composing existing primitives
(`agent_ops.go:16-129`) + the canned script mechanism
(`fixture.go:92-103` `WithCannedScript`):

1. Multi-peer fan-out: 3 agents in one channel, OWNER_ONLY policy assert
   (`proto/compass/v1/comms.proto:239-241,252-257`), mention-steer vs
   subscriber-deliver at N>2.
2. Provision/enroll hardening: runner restart with live sessions re-enrolls
   and resumes delivery; provision with an invalid runner token fails closed.

**The PR-submitting scenario is split OUT of this task.** It was previously
scoped here as "hard-gated on the compass-agent forge-tool lane" — one
missing layer. The gap is actually FOUR layers of the forge execution stack:
(1) the agent-side forge tool (`packages/compass-agent/src/` has only the
`comms.ts`/`lifecycle.ts` brokers — no forge tool); (2) the Runner's
`AgentGateway.Forge` handler; (3) the Server's `RelayForgeCall` hub leg —
`go/internal/runnerhub/` has `relay_comms.go`/`relay_lifecycle.go`/
`relay_board.go` but no `relay_forge.go`, and the Handler embeds
`UnimplementedRunnerServiceHandler` (`handler.go:44-45`), so a
`RelayForgeCall` today returns `CodeUnimplemented` (`go/internal/gen/compass/v1/compassv1internalconnect/runner.connect.go:609-611`)
— the FIRST failure on any forge path, well before any tool dispatch; and
(4) the server-side forge WRITE executor + credential path the relay would
delegate to (`go/server/board.go:73-74,108-109` — "no real forge-tracker
write seam exists yet"; `go/internal/forge` ingest is read-only polling). A
scenario gated on a four-layer cross-lane stack is not a shippable task here:
it moves to a stub referencing the forge execution stack's lane (DL-052 — the
Server holds the sole forge write credential) as THAT lane's acceptance test.
The fake-forge fixture helper sketched for it lands with that lane, not this
record.

Interfaces:

- Consumes: `Fixture` primitives (`go/e2e/agent_ops.go:16-129`);
  `WithCannedScript(script ...CannedTurn)` (`go/e2e/fixture.go:98-103`);
  persistent-site restart substrate `newPersistentSite`/`WithSite`
  (`go/e2e/fixture.go:105-117`).
- Produces: two green scenarios in the deterministic tier; the stub task
  handing the PR-submitting scenario — with its four-layer gap named — to
  the forge-stack lane (DL-052).

Test cycle: red — no scenario exercises multi-peer fan-out or
restart-re-enroll today; both are unexercised compositions. Green — both
pass twice back-to-back (the leg-6 idempotence gate, `legsix_test.go:14-31`)
under `-race`.

### B4 [harness] — scenarios: home-channel first-turn + restart-replay

Two scenarios landing AFTER #256/#303 merge (Global Constraints sequencing):

1. Home-channel first-turn as a first-class scenario (not a leg detail):
   CreateAgent → Provision → Start idle → PostMessage to
   `AgentAccount.home_channel_id` (`proto/compass/v1/comms.proto:174-175`) →
   delivery fan-out → `AwaitSessionSettled` → the reply lands back in the
   home channel. Reuses #256's delivery-cursor event-gate as a shared
   primitive (promote it out of the leg-5 test into `agent_ops.go`).
2. Restart-replay: mid-conversation runner restart; assert live ops are held
   behind the replay barrier until `replay_complete_ack`
   (`proto/compass/v1/agent.proto:64-67`) and the conversation resumes with
   no duplicate/lost delivery.

Interfaces:

- Consumes: #256's re-modeled turn legs + delivery-cursor gate (its
  `go/e2e/agent_ops.go` additions); #303's fresh-start `replay_complete`
  emission; `AgentControl.replay_complete` / `AgentFrame.replay_complete_ack`
  (`proto/compass/v1/agent.proto:64-67,166-167`); `WithSite` restart
  substrate (`go/e2e/fixture.go:105-117`).
- Produces: the two scenarios green in the deterministic tier; the
  delivery-cursor wait as a named fixture primitive with a doc comment
  pinning its contract ("the cursor advances on the agent `delivery_ack`, not
  on session settle", pr://256 body).

Test cycle: red — no scenario drives a restart across a live conversation;
the replay barrier is proven only at the seam level. Green — both pass under
`-race`, double-run clean.

### B5 [platform] — PR result surfacing: step summary, artifacts, sticky comment

Make the e2e verdict readable without pulling code: (1) the e2e CI step (from
PR #256) writes a per-scenario table to `$GITHUB_STEP_SUMMARY` (scenario name,
verdict, duration — derived from `go test -json` output by a small bun/TS
tool under `tools/`, per scripts-over-bash); (2) on failure, upload the
fixture's short-root logs + session transcripts as an artifact via
`actions/upload-artifact`; (3) ONE sticky PR comment (created-or-updated by
workflow, keyed by a marker) that carries: the e2e verdict + run link,
artifact links, and the PR-preview link when the isolated PR-preview
environment has published one. The comment needs `pull-requests: write`,
which the CI job deliberately lacks (`ci.yml:76-77` `contents: read`) — so
the comment rides a separate minimal-permission workflow triggered by
`workflow_run` completion, never by granting the gate job write scopes.

Interfaces:

- Consumes: `go test -json` stream from the e2e step;
  `actions/upload-artifact`; `workflow_run` trigger +
  `pull-requests: write`-scoped comment API. Two `workflow_run` facts the
  implementer must design around: (1) the `workflow_run` payload does NOT
  reliably carry the PR number (the `pull_requests` array is empty for
  fork-originated runs) — the triggering CI run must export the PR number as
  an artifact the comment workflow downloads; (2) that downloaded artifact
  is UNTRUSTED input (the comment workflow runs with `pull-requests: write`
  on content a PR produced) — validate it as strictly numeric and never
  interpolate it into anything but the comment API's issue-number field.
- Produces: `tools/e2e-summary` (bun/TS: parse `go test -json` → markdown
  table; unit-tested pure construction per the tools convention);
  the sticky-comment workflow; artifacts retained 14 days.

Test cycle: red — today a failure is a raw check log; green — a seeded
failing scenario produces the summary table, the artifact bundle, and one
updated (not duplicated) comment across two pushes.

### B6 [platform] — the capture stack: headless-browser tooling for visual evidence

The prerequisite that activates the skill's phase 2 (A3). No headless-browser
or capture tooling exists anywhere in the compass tree (verified this
session: no playwright/puppeteer/chromium under `apps/`/`tools/`/
`packages/`), and UI tests run under happy-dom, not a real browser
(`apps/ui/test-setup.ts:1-4` — "Bun's runner is otherwise headless"). The
stack: playwright + chromium available to the agents, plus a `tools/capture`
wrapper (bun/TS, per scripts-over-bash) that drives a target URL (the PR
preview, or a locally-served build) and writes a PNG/webm.

**Attach mechanism (chosen explicitly):** evidence is committed to a
dedicated orphan `pr-assets` branch and linked by stable
`raw.githubusercontent.com` per-commit URL — chosen over GitHub's
markdown-image CDN (a browser-session upload flow with no clean headless
API) and over a separate asset repo (an extra cross-repo write credential);
pruning merged-PR assets rides this same task.

Interfaces:

- Consumes: a running UI to capture (the PR preview URL, or a local
  `vite`-served build); playwright's chromium driver.
- Produces: `tools/capture` (bun/TS: `capture <url> --screenshot out.png` /
  `--record out.webm`); the `pr-assets` orphan-branch attach convention; the
  phase-2 activation signal for the bundled skill (B7 flips the skill's
  phase-2 clause from "gated on B6" to active).

Test cycle: red — no tool can produce a screenshot from a running build
today; green — `tools/capture` against the dev stack's UI yields a PNG that
shows the rendered app, and the attach flow produces a stable raw URL that
renders in a PR body.

### B7 [platform] — the bundled PR-validation skill

Author the validation skill decided in A3 and bundle it with the Compass
agents by default: the skill ships as part of the standard agent equipment
Compass provisions, so every agent operating in this repo carries it with no
per-user or per-host setup. This replaces the earlier personal-dotfiles
delivery entirely (A3's ruling).

Skill content per A3: the evidence decision tree, the click-path template
(phase 1), capture + attach mechanics (phase 2, referencing B6's
`tools/capture`), the e2e invocation and result-reporting shape (B5's PR-body
conventions), and the always-capture-where-applicable requirement for
screenshots/recordings.

Interfaces:

- Consumes: A3's ruled content; B5's PR-body/result conventions; B6's
  `tools/capture` + attach mechanism (phase 2).
- Produces: the skill artifact in the compass repo, wired into the default
  agent provisioning surface so it is present in every Compass agent's skill
  set; the phase-1/phase-2 clause keyed to B6's landing.

Test cycle: red — no bundled guidance requires validation evidence, and PRs
land evidence-less today; green — a freshly provisioned Compass agent lists
the skill, and a probe PR authored under it carries the phase-1 evidence
section (click-path + named e2e scenarios or suite result).

### B8 [platform] — path-filtered live-model e2e leg in GitHub Actions

The A4 ruling's second tier. A separate workflow (one-job doctrine: a
least-privilege sibling, never a required check) triggered on `pull_request`
with a `paths:` filter, running the e2e suite in live-model mode with the LLM
key injected from a GitHub Actions secret. Proposed filter globs:

```yaml
on:
  pull_request:
    paths:
      - 'go/e2e/**'
      - 'go/internal/runner/**'
      - 'go/internal/runnerhub/**'
      - 'packages/compass-agent/**'
      - 'proto/compass/v1/agent*.proto'
```

The deterministic canned tier is UNAFFECTED: it remains the required
every-PR gate, and this leg adds live-model coverage only where the diff can
plausibly change model-facing behavior. Fork PRs never receive the secret
(GitHub withholds secrets from fork-PR events by default; the workflow
additionally guards on same-repo head).

Interfaces:

- Consumes: the e2e suite's live-model mode (the harness runs against a real
  provider when canned mode is not selected — the canned backend is an
  explicit fixture option, `go/e2e/fixture.go:76-103`, so its absence is the
  live path); a GitHub Actions secret carrying the LLM key; the `paths:`
  filter globs above.
- Produces: the live-leg workflow YAML; its verdict surfaced through the same
  B5 summary/comment plumbing (a second table section, marked live-tier).

Test cycle: red — no live-model leg exists in CI; green — a PR touching
`go/e2e/**` triggers the leg and it settles a real model turn; a docs-only PR
does not trigger it; the canned gate result is identical in both cases.

## Tasks

- [ ] B1 [compass-ui] configurable UI build: `VITE_COMPASS_BASE_URL`/`VITE_COMPASS_TOKEN` → a `dist` that boots against a configured TLS door (red-green vs the devenv TLS door); consumed by the isolated PR-preview environment (private infra design repo)
- [ ] B3 [harness] scenarios: multi-peer fan-out + provision/enroll hardening (double-run + `-race` green); PR-submitting scenario split out as a stub handed to the forge-stack lane (DL-052) with its four-layer gap named
- [ ] B4 [harness] scenarios: home-channel first-turn (delivery-cursor gate promoted to a primitive) + restart-replay barrier — lands after #256/#303 merge
- [ ] B5 [platform] result surfacing: `tools/e2e-summary` (go test -json → step-summary table), failure artifacts, sticky PR comment via a minimal-permission `workflow_run` workflow (PR number via artifact, treated as untrusted)
- [ ] B6 [platform] capture stack: playwright + chromium + `tools/capture` (PNG/webm from a running build) + the `pr-assets` attach convention
- [ ] B7 [platform] the bundled PR-validation skill: authored in-repo, provisioned to every Compass agent by default (phase 1 now; phase 2 activates with B6)
- [ ] B8 [platform] path-filtered live-model e2e leg: separate GitHub Actions workflow, `paths:`-gated, LLM key from an Actions secret, never required; canned tier unchanged as the every-PR gate

## Resolved decisions

Every fork in this record is ruled; nothing here blocks on an answer.

1. **The validation skill is bundled with the Compass agents (A3) — RULED by
   Matt.** Not personal dotfiles, not an external config spine, not
   AGENTS.md prose alone: Compass ships the skill to its agents by default,
   and agents always capture screenshots/recordings where applicable. This
   also dissolves the earlier scope question (compass-only vs every repo):
   the skill is Compass equipment, scoped by construction to agents working
   here.
2. **Live-model e2e placement (A4) — RULED by Matt.** A path-filtered GitHub
   Actions leg with an Actions-secret key; the credential-free canned tier
   stays the required every-PR floor. This supersedes the earlier
   deferral between deterministic-only pre-submit and nightly-only live runs.

## Open Questions

Non-load-bearing deferrals only; each carries a recommendation the record
designs against, so no task blocks on an answer.

1. **Capture-stack delivery: where do playwright/chromium live?**
   (non-load-bearing). B6 needs a real browser available to agents. Arms:
   (a) in the agent image — uniform for every agent, but grows the image and
   makes B6 a cross-record dependency on the image-publishing lane
   (`publish-agent-image.yml`); (b) a host-level install where agents run —
   smaller image blast radius, but only agents on prepared hosts can
   capture. **Recommend (a)**; either way the skill's phase 1 ships now and
   phase 2 activates when this lands.

---

Ledger-impact: none — platform-corpus record, outside the
design-ledger-gate's product scope (`tools/design-ledger-gate/index.ts:45`).
