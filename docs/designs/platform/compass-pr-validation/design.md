# Compass PR validation — fast review + agent-self-tested coverage

> **Design record (Record B of the dogfood-operations set).** How a Compass PR
> gets validated without Matt pulling a branch: the PR's full stack deployed
> to the isolated `preview` env for UI-visible changes, and an expanded
> agent-self-run e2e harness for behavioral changes, with results surfaced ON
> the PR. Consumes
> Record A's environment contract
> (`docs/designs/platform/compass-dogfood-operations/design.md`: the `main` and
> `preview` envs on `mattfw`, creds via DL-026, config via DL-078) by reference
> — envs are not redesigned here. Extends the existing e2e harness record
> (`docs/designs/platform/compass-dogfood-e2e/design.md`, SEA-1681, H1-H8).
> Local-dev / gate mechanics are Record C
> (`docs/designs/platform/compass-local-dev/design.md`).

Status: **Draft** — pending the design-PR gate.

## Problem / Intent

Agents submit Compass PRs; Matt's review cycle is the bottleneck. A
user-visible change today forces Matt to pull the branch, build, and click
through it; a behavioral change forces him to trust the diff. The intent
(DECIDED#1/#8): maximize what agents end-to-end test THEMSELVES, and give Matt
a one-click review surface — a preview link for UI changes, a
pass/fail+artifacts summary on the PR for behavioral ones — so review time is
judgment, not reproduction. The repo is public, so every preview surface must
be access-gated (tailnet) and no PR server code may ever touch the live wave.

## Approach

Two lanes, split by change type (DECIDED#8; its Lane-1 arm re-ruled by Matt
to full-stack-on-preview — see Alternatives considered), plus the agent-side
requirement that makes the lanes get used:

```text
  PR opened ──┬─ UI-visible?  ──► Lane 1: full PR stack → `preview` env
              │                   (selected by `preview` label, per Record A)
              │                   → tailnet-gated link on the PR
              └─ behavioral?  ──► Lane 2: expanded e2e harness (deterministic
                                  full-stack tier, agent-authored scenarios)
                                  → per-PR CI gate + summary + artifacts ON the PR
  either way ────────────────► agent PR-validation skill + AGENTS.md rule
                                  (personal/matt/agents): validation evidence
                                  (preview link + click-path now; capture once
                                  the B6 stack ships) before review-ready
```

### A1 — Lane 1: full PR stack on the `preview` env

**What runs where — the safety invariant first.** The `main` env never runs
PR code (Record A, DECIDED#2) — and under this lane it never has to: NO PR
artifact touches `main` at all. PR code runs on the `preview` env, the ONE
place Record A permits PR server/runner/agent code to run
(`compass-preview.service` "may be torn down/redeployed freely",
`compass-dogfood-operations/design.md:54`). Lane 1 deploys the PR's WHOLE
stack — server, runner, agents, UI — to `preview`, and the preview UI dials
PREVIEW's own door (ports `50151`/`50161`,
`compass-dogfood-operations/design.md:51`), never `main`'s. This previews the
repo's dominant PR shape (proto+server+UI moving together) end to end: the
reviewer uses the actual feature, not a diff imagined.

**Preview mints its own creds.** The preview stack authenticates against its
own disposable env: Record A's dedicated `compass-preview` user + keyring
holds preview's scoped credential set, and the reviewer bearer the preview UI
carries is minted ON preview, against preview's door. It carries no authority
over `main` and reaches no live work channels — the whole
baked-bearer/live-wave-authority analysis the static-bundle arm required
(agent-kind vs user-kind accounts, the admin-gate preview surface, CSP/egress
hardening toward `main`'s door) is gone with that arm (see Alternatives
considered — the OQ1-3 fold). Because the bearer's authority ends at a
disposable env, it MAY be admin-scoped, which makes the admin chrome (fleet
status, spawn, session lifecycle) previewable too — something a live-wave
bearer could never safely carry. What still applies from the old posture:

- **Tailnet-gated, always.** The preview surface is exposed ONLY via
  `tailscale serve` (tailnet-internal HTTPS; never `tailscale funnel`, which
  is public). The repo is public; the preview is not — reaching it requires
  tailnet membership (Matt's devices and the fleet).
- **Fork PRs never deploy.** The deploy workflow acts only for same-repo
  branches (`github.event.pull_request.head.repo.full_name ==
  github.repository`); a fork PR can never reach the preview env or the
  deploy credentials.

**PR selection — the `preview` label, single-holder.** One shared `preview`
env cannot auto-deploy every open PR: last-synchronize-wins across N open PRs
is serialization chaos, and Matt's requirement rules it out explicitly
("preview env but there needs to be some way of choosing which PR goes to
preview"). The env has ONE occupant at a time, chosen deliberately. The
mechanism (delegated by Matt to this record):

- **Claim.** A PR carrying the **`preview` GitHub label** is the current
  occupant of the shared env. The deploy workflow triggers on `pull_request`
  `labeled` + `synchronize` events but ACTS only for the PR that currently
  holds the label — a synchronize on an unlabeled PR is a no-op.
- **Single-holder, last-claim-wins-but-explicit.** When a PR is labeled
  `preview`, the workflow removes the label from any other PR holding it (so
  exactly one PR owns the env) and posts a sticky comment on the displaced PR
  noting it lost the env. Claiming is a deliberate human/agent act, never
  automatic.
- **Release.** `unlabeled` or PR `closed` releases the env — the preview
  service stops (`compass-preview.service` down until a claim (re)starts it);
  the cheapest release, and a claim already redeploys from scratch.
- **Deploy action.** Check the PR ref out into Record A's
  `~/compass-envs/preview` checkout
  (`compass-dogfood-operations/design.md:48`) and restart
  `compass-preview.service` (Record A: it "may be torn down/redeployed
  freely"); the PR's UI build (B1) points at preview's own door.

This selection mechanism is what makes a single shared env workable without
N-PR clobbering.

**Review surface.** The reviewer opens the preview from a sticky PR comment
carrying the tailnet-gated link (B5's comment surface, shared with Lane 2).
A UI change that also needs new server behavior — the dominant PR shape — is
previewable HERE, before review, because the PR's server half runs on
preview; it no longer waits for the server half to merge.

### A2 — Lane 2: the expanded e2e harness, agent-self-run

**Base.** The harness exists and is merged: `go/e2e` is the podman-tagged
package over `stack.Up` (fixture `go/e2e/fixture.go:33-52`, primitives
`go/e2e/agent_ops.go:14-129`: `CreateAgent`/`Provision`/`StartSession`/
`Resume`/`AwaitSessionSettled`/`RemoveWorkspace`), with the deterministic
canned-model backend (`WithCannedModel`/`WithCannedScript`,
`fixture.go:71-98`) and the cross-restart teardown/idempotence leg
(`legsix_test.go:14-31`). The per-PR CI gate is H8's lane, wired by PR #256
("adds the `Dogfood e2e (deterministic full-stack tier)` step to `ci.yml` …
then `go test -tags podman ./e2e/...` under `-race`", pr://256 body; stacked
on #303's `replay_complete` emission, must-not-merge-before). This record
EXTENDS that harness — new scenarios compose the existing primitives (the
harness record's own contract: "New scenarios … compose the same primitives —
the scenario set grows without touching the core",
`docs/designs/platform/compass-dogfood-e2e/design.md:379-381`).

**Target scenario set** (each = one scenario file in `go/e2e`, composing
fixture primitives; coverage state grounded):

| Scenario | State today | What the expansion adds |
| --- | --- | --- |
| Multi-agent communication | Legs 3+4 merged: agent-driven spawn + `@mention` steer/deliver split (harness record H4, `design.md:634-665`) | Multi-peer fan-out: 3+ agents, channel policy (OWNER_ONLY, `comms.proto:233-235,247-250`), unmentioned-subscriber deliver at scale |
| PR submitting | Wire contract exists on paper only — `AgentGateway.Forge` carries the envelopes (`proto/compass/v1/agent_gateway.proto:79-90`) and the relay procedure is generated (`RunnerServiceRelayForgeCallProcedure`, `runner.connect.go:74-77`) — but FOUR execution layers are missing: no agent-side forge tool (`packages/compass-agent/src/` has only the `comms.ts`/`lifecycle.ts` brokers); no Runner `Forge` handler; no Server relay leg (`go/internal/runnerhub/` has `relay_comms.go`/`relay_lifecycle.go`/`relay_board.go`, no `relay_forge.go`; the Handler embeds `UnimplementedRunnerServiceHandler`, `handler.go:44-45`, so `RelayForgeCall` returns `CodeUnimplemented` today, `runner.connect.go:610`); no server-side forge write executor (`go/server/board.go:73-74,108-109` — "no real forge-tracker write seam exists yet") | Deferred — split out of B3: the scenario belongs to the forge execution stack's own lane (DL-052) as ITS acceptance test (see B3) |
| Provision/enroll | Leg 1 + leg 6 merged: bring-up through enrollment, teardown idempotence across a stack restart (`legsix_test.go:14-31`) | Re-enroll after runner restart with live sessions; provision under a revoked/expired runner token (fail-closed assert) |
| Home-channel first-turn | Re-modeled by PR #256: "legs 2 / 3-4 / 5 now drive the first turn by posting to the agent's home channel (the post-`initial_prompt` contract), delivered via the live fan-out" (pr://256 body). `initial_prompt` is already removed on this checkout — `reserved` at `compass.proto:590` and `:613` (see `compass-initial-prompt-removal.md`) — so the home-channel-first-turn delivery contract is the CURRENT state, not a pending re-model; the home channel is minted at CreateAgent ("The agent's home channel, minted at CreateAgent. The agent is always subscribed to it", `comms.proto:166-169`) | First-class scenario: post-to-home-channel → delivery → first turn settles → reply lands back in the home channel; the delivery-cursor event-gate #256 introduces (its leg-5 hardening) becomes a shared primitive |
| Replay | Replay-then-Live subscribe waits exist (`SubscribeComms`, harness record A2); `replay_complete` is the agent control-lane barrier ("on receipt the Runner releases the live ops held behind the restart replay barrier", `proto/compass/v1/agent.proto:64-67`), fresh-start emission lands with #303 | A restart-replay scenario: kill/restart the runner mid-conversation, assert the replay barrier holds live ops until `replay_complete_ack`, no duplicate or lost delivery across the barrier |

**How agents self-run them.** Two surfaces, both existing-mechanism:

1. **CI (deterministic, gating).** #256's `ci.yml` step runs the full suite on
   every PR — this is the floor every PR gets with zero agent effort, riding
   the affected-detection gate (`moon ci :ci` on PRs,
   `.github/workflows/ci.yml:271-282`) plus the dedicated e2e step.
2. **`preview` env (live-model, pre-submit).** The submitting agent runs the
   suite itself against its branch before opening the PR: `go test -tags
   podman ./e2e/...` in its own workspace (the fixture compiles the child
   binaries from the tree and stands up a private stack under a short root,
   `fixture.go:127-140`, so nothing touches `main`), optionally in live-model
   mode with creds injected per Record A's DL-026 surface. The PR-validation
   rule (A3) makes this a submit-gate the agent owns, not an optional extra.

**How results surface ON the PR.** Three surfaces, one task (B5):

- the required check itself (#256's step) — pass/fail;
- `$GITHUB_STEP_SUMMARY` — a per-scenario table (name, verdict, duration)
  written by the e2e step, visible on the check page without opening logs;
- artifacts — on failure, the step uploads the stack's state dir logs +
  per-scenario transcripts via `actions/upload-artifact`, and the sticky PR
  comment (shared with Lane 1's preview link) links the run + artifacts, so
  Matt reads verdict → summary → artifacts without pulling code.

### A3 — The agent PR-validation requirement (skill + rule)

A new skill and rule land in `personal/matt/agents/` — Matt's dotfiles repo,
"authored in `personal/matt/agents/`, symlinked into `~/.agents/` by nix"
(`~/.agents/AGENTS.md:125`) — NOT in the compass repo. Conventions grounded
from the live tree: rules are flat `~/.agents/rules/<name>.md` files with a
frontmatter `description:` (`~/.agents/rules/planning-evidence.md:1-3`);
skills are `~/.agents/skills/<name>/SKILL.md` with `name:`/`description:`
frontmatter (`~/.agents/skills/design/SKILL.md:1-4`).

- **Rule `pr-validation.md`** (always-applied class, PHASED): any PR with a
  user-visible change MUST carry validation evidence before it is
  review-ready. Phase 1 (immediate): the Lane 1 preview link plus a described
  click-path (what to open, what to click, what changed) — evidence agents
  can produce today. Phase 2 (activates when B6's named capture prerequisite
  ships): a screenshot (static UI change) or screen recording
  (interaction/motion change) captured from the actually-running artifact
  (the Lane 1 preview, or a locally-driven build), never a mockup.
  Behavioral changes MUST state which e2e scenarios cover them (or run the
  suite per A2.2 and say so). Composes with the existing `pre-finish-checks`
  rule; it does not duplicate format/lint/test.
- **Skill `compass-pr-validation/SKILL.md`** (the how): decide the lane
  (UI-visible vs behavioral), drive the preview or the local UI, write the
  click-path section (phase 1), capture and attach evidence once the capture
  stack exists (phase 2 — B6 names it as a prerequisite; no headless-browser
  tooling exists in the tree today), run/report the e2e suite, and write the
  validation section of the PR body. The skill is the reference; the rule is
  the requirement.

## Global Constraints

- **`main` never runs PR code** (Record A env contract, DECIDED#2). Lane 1
  deploys the full PR stack to the ISOLATED `preview` env — the one place
  DECIDED#2 permits PR server/runner/agent code to run; no PR artifact of any
  kind touches `main`. Lane 2 runs PR code only in the fixture's private
  stack.
- **Public repo ⇒ access-gated previews.** Preview surfaces are reachable only
  on the tailnet (`tailscale serve`, never `funnel`); fork PRs never publish a
  preview and never reach deploy credentials (the same posture as
  `publish-agent-image.yml:8-9`: "PR events never reach it at all — no token or
  secret is ever exposed to a fork PR").
- **Bearer over TLS only.** The preview UI dials PREVIEW's network door (TLS
  1.3 minimum, `go/server/network_door.go:112-119`) with a bearer minted on
  preview; the loopback dev door is never exposed off-box
  (`network_door.go:70-71` enforces loopback).
- **Preview owns its CORS origin.** The network door exposes exactly one
  configured origin (`network_door.go:141-149`); the PREVIEW server's slot is
  set to the preview UI's origin. `main`'s single CORS slot is NOT spent by
  this record.
- **One-job CI doctrine.** New CI wiring extends steps inside the existing
  `CI` job or a least-privilege sibling workflow, never a project-enumerating
  matrix (`.github/workflows/ci.yml:4-18`); the preview-deploy job follows the
  `publish-agent-image.yml` separate-workflow precedent (least privilege, own
  concurrency, off the required-check hot path).
- **Event-gated determinism.** New e2e scenarios follow the harness's
  discipline — no sleeps, no retries; assertions wait on bus subscriptions,
  store reads, or session-frame streams (harness record Global Constraints,
  `compass-dogfood-e2e/design.md:81-88`; the merged `AwaitSessionSettled` is
  "FULLY EVENT-GATED", `go/e2e/agent_ops.go:84-90`).
- **Podman-tagged, skip-not-fail.** New scenarios live in `go/e2e` under
  `//go:build podman` with the `podmanUsable()` skip guard
  (`go/e2e/fixture.go:1,473-476`).
- **Scripts over bash.** Any deploy/summary logic beyond a one-liner is a
  bun/TS tool under `tools/` (the `~/.agents/AGENTS.md:45-47` no-bash-gate
  posture), not shell in YAML.
- **Skill/rule conventions.** The skill/rule artifacts follow the live
  `~/.agents` shapes (rule: flat `.md` + `description:` frontmatter; skill:
  `<name>/SKILL.md` + `name:`/`description:` frontmatter) and are authored in
  `personal/matt/agents/`, delivered fleet-wide via DL-078 (Record A's config
  spine).
- **Sequencing floor.** Lane 2 tasks that extend the CI gate land AFTER #256
  (the `[ci]` gate wiring) and #303 (`replay_complete` fresh-start emission)
  merge; the replay scenario (B4) additionally consumes #303's emission as its
  subject.
- Commits authored as seal with Matt's co-author trailer
  (rule://commit-conventions); records here are platform-corpus, so the
  design-ledger-gate does not govern them (`tools/design-ledger-gate/index.ts:45`
  scopes `PRODUCT_DIR = "docs/designs/product"`; a platform path "is not a
  record", `index.test.ts:124-126`).

## Plan

Owner lanes: `[platform]` = mattfw host + CI wiring; `[compass-ui]` = apps/ui;
`[harness]` = go/e2e; `[agents-repo]` = personal/matt/agents (Matt's dotfiles
— a separate repo/PR). (The `[compass-agent]` lane left the plan when the
PR-submitting scenario split out to the forge-stack lane — B3.)

### B1 [compass-ui] — previewable UI build: env injection for the preview target

Make the UI bundle buildable against the preview env's door+bearer: a
`.env.preview`-style build input (or CI-injected env) setting
`VITE_COMPASS_BASE_URL` to PREVIEW's network-door URL (port `50161` per
Record A) and `VITE_COMPASS_TOKEN` to the reviewer bearer minted ON preview.
No code change to connection resolution is expected — `resolveConnection`
already normalizes an absent token to a no-auth client and takes any base URL
(`apps/ui/src/live/connection.ts:45-58`). Serving path: with the
single-holder `preview` label (§A1) there is at most ONE active preview, so
the per-PR sub-path scheme (`/pr/<N>/` + a Vite `base` per build) is
unnecessary — the build serves from one stable `preview` URL (the
label-holder's), at the root path. No `base` wiring needed (today
`vite.config.ts:7-11` sets no `base`, and none is required at the root).

Interfaces:

- Consumes: `resolveConnection(env: CompassEnv): Connection` with
  `CompassEnv{VITE_COMPASS_BASE_URL?, VITE_COMPASS_TOKEN?}`
  (`apps/ui/src/live/connection.ts:31-32,45`); moon task `compass-ui:build` =
  `bunx vite build` → `dist` (`apps/ui/moon.yml:19-23`); Record A's `preview`
  network-door address (`…:50161`); `IssueTokenRequest{account_id} →
  IssueTokenResponse{token}` (`proto/compass/v1/compass.proto:663-675`) for
  the reviewer-token mint ON the preview env, minted against a preview admin
  account (so the admin chrome is previewable — the bearer's authority ends at
  the disposable env); the mint is re-runnable on every deploy — the env is
  disposable, so the token is too.
- Produces: `moon run compass-ui:build` parameterized by
  `VITE_COMPASS_BASE_URL`/`VITE_COMPASS_TOKEN` yielding a `dist/` that boots
  against preview's network door.

Test cycle: red — no build input targets a configured TLS door today; green —
`bunx vite build` with the preview env yields a `dist/` whose
`bootConnection` reaches WhoAmI against a TLS door (verified once against the
dev stack's own network door, which devenv already binds:
`devenv.nix:255-257` `--listen … --tls-cert … --tls-key`).

### B2 [platform] — preview deploy workflow: full-stack deploy + label lifecycle

The label-triggered deploy workflow plus the serving front. On `pull_request`
`labeled`/`synchronize`/`unlabeled`/`closed` events (same-repo branches only
— the fork guard), acting only for the current `preview`-label holder (§A1):

- **Deploy:** check the PR ref out into Record A's `~/compass-envs/preview`
  checkout and (re)start `compass-preview.service` (Record A: `devenv up` on
  that checkout; the unit "may be torn down/redeployed freely"). The runner
  is a tailnet node or pushes via a deploy door on `mattfw` — either way the
  deploy credential is never exposed to fork PRs.
- **UI:** build the PR's UI (B1) against preview's door and serve it on
  `mattfw` via `tailscale serve` (tailnet-internal HTTPS; never `funnel`) at
  the one stable preview URL.
- **Label lifecycle (same-repo claims only).** On `labeled` of a SAME-REPO
  PR, strip the `preview` label from any other PR holding it and post the
  displacement sticky comment; on `unlabeled`/`closed`, release the env (stop
  `compass-preview.service`; the next claim (re)starts it). A FORK PR's claim
  is rejected up front — on `labeled` of a fork PR, immediately remove the
  `preview` label and post a "preview is same-repo only" comment WITHOUT
  displacing the current holder or deploying, so a fork label never evicts a
  live incumbent.

A separate least-privilege workflow (the `publish-agent-image.yml`
precedent), never a required check — a preview flake must not red the merge
gate; the label lifecycle needs `pull-requests: write`, which stays confined
to this workflow and never reaches the CI gate job. Isolation is Record A's,
not this task's: preview already runs as the dedicated `compass-preview` user
under a resource-capped slice, with its own keyring, postgres, and TLS
keypair — so the main-facing hardening the static-bundle arm carried (a CSP
pinning `connect-src` to `main`'s door; spending `main`'s single CORS slot on
a preview origin) is dropped with that arm. The PREVIEW server sets its OWN
`CORSAllowedOrigin` to the preview UI's origin; `main`'s slot stays free.

Interfaces:

- **Serialization (REQUIRED):** the workflow carries its own
  `concurrency: { group: compass-preview-deploy, cancel-in-progress: false }`
  (the `publish-agent-image.yml:14-15` precedent) so claim → label-strip →
  checkout → restart against the ONE shared `~/compass-envs/preview` tree runs
  strictly one-at-a-time; a single claim's label-strip + deploy is one
  serialized unit. Without it, two near-simultaneous `preview` events race the
  shared checkout and can strip each other's label (split-brain).
- Consumes: B1's parameterized build; Record A's `preview` env contract
  (the `~/compass-envs/preview` checkout, `compass-preview.service`, doors
  `50151`/`50161`, the `compass-preview` user + slice); tailnet reachability
  of `mattfw` (Record A); a deploy credential scoped to the preview checkout
  (implementer's choice: a tailnet-only push target or a deploy door; NEVER
  a repo secret exposed to fork PRs — guard
  `github.event.pull_request.head.repo.full_name == github.repository`); the
  GitHub label API under `pull-requests: write` for the single-holder
  lifecycle.
- Produces: one tailnet-gated preview URL serving the label-holder's full
  stack; the claim/displace/release label lifecycle (displacement sticky
  comment included); release on unlabel/close; the sticky-comment link
  payload consumed by B5.

Test cycle: red — no preview deploy exists; green — label a test PR
`preview`: its full stack comes up on the preview env and the link renders
the PR's UI against preview's door (admin chrome included — preview's own
creds, no live-wave gate); label a SECOND PR: the first is displaced (label
stripped, comment posted) and the env redeploys to the second; a non-tailnet
client cannot reach the URL; unlabeling or closing releases the env.

### B3 [harness] — scenario expansion: multi-peer fan-out, provision/enroll hardening

Two new scenario files in `go/e2e`, composing existing primitives
(`agent_ops.go:14-129`) + the canned script mechanism
(`fixture.go:87-98` `WithCannedScript`):

1. Multi-peer fan-out: 3 agents in one channel, OWNER_ONLY policy assert
   (`comms.proto:233-235,247-250`), mention-steer vs subscriber-deliver at N>2.
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
`RelayForgeCall` today returns `CodeUnimplemented`
(`runner.connect.go:610`) — the FIRST failure on any forge path, well before
any tool dispatch; and (4) the server-side forge WRITE executor + credential
path the relay would delegate to (`go/server/board.go:73-74,108-109` — "no
real forge-tracker write seam exists yet"; `go/internal/forge` ingest is
read-only polling). A scenario gated on a four-layer cross-lane stack is not
a shippable task here: it moves to a stub referencing the forge execution
stack's lane (DL-052 — the Server holds the sole forge write credential) as
THAT lane's acceptance test. The fake-forge fixture helper sketched for it
lands with that lane, not this record.

Interfaces:

- Consumes: `Fixture` primitives (`go/e2e/agent_ops.go:18-129`);
  `WithCannedScript(script ...CannedTurn)` (`go/e2e/fixture.go:93-98`);
  persistent-site restart substrate `newPersistentSite`/`WithSite`
  (`go/e2e/fixture.go:100-112,441-450`).
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
   `AgentAccount.home_channel_id` (`comms.proto:166-169`) → delivery fan-out →
   `AwaitSessionSettled` → the reply lands back in the home channel. Reuses
   #256's delivery-cursor event-gate as a shared primitive (promote it out of
   the leg-5 test into `agent_ops.go`).
2. Restart-replay: mid-conversation runner restart; assert live ops are held
   behind the replay barrier until `replay_complete_ack`
   (`proto/compass/v1/agent.proto:64-67`) and the conversation resumes with
   no duplicate/lost delivery.

Interfaces:

- Consumes: #256's re-modeled turn legs + delivery-cursor gate (its
  `go/e2e/agent_ops.go` additions); #303's fresh-start `replay_complete`
  emission; `AgentControl.replay_complete` / `AgentFrame.replay_complete_ack`
  (`agent.proto:64-67,171-172`); `WithSite` restart substrate
  (`fixture.go:100-112`).
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
fixture's short-root logs + session transcripts as an artifact; (3) ONE
sticky PR comment (created-or-updated by workflow, keyed by a marker) that
carries: Lane 1 preview link (when B2 published one), e2e verdict + run link,
artifact links. The comment needs `pull-requests: write`, which the CI job
deliberately lacks (`ci.yml:76-77` `contents: read`) — so the comment rides a
separate minimal-permission workflow triggered by `workflow_run` completion,
never by granting the gate job write scopes.

Interfaces:

- Consumes: `go test -json` stream from the e2e step; B2's preview URL;
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

### B6 [agents-repo] — the PR-validation rule + skill in personal/matt/agents

Author `rules/pr-validation.md` and `skills/compass-pr-validation/SKILL.md`
in `personal/matt/agents/` (its own PR on that repo — NOT this repo), per A3.

**The rule is PHASED, because the capture stack does not exist yet.** No
headless-browser or capture tooling exists anywhere in the compass tree
(grep this session: no playwright/puppeteer/chromium under
`apps/`/`tools/`/`packages/`), and UI tests run under happy-dom, not a real
browser (`apps/ui/test-setup.ts:1-4` — "Bun's runner is otherwise
headless"). A rule mandating "screenshot from the actually-running artifact"
on day one would demand evidence agents cannot produce. So:

- Phase 1 (lands with this task): user-visible change ⇒ the Lane 1 preview
  link + a described click-path in the PR body; behavioral change ⇒ name the
  covering e2e scenarios or run the suite and state the result.
- Phase 2 (activates when the capture prerequisite ships): screenshot
  (static change) or recording (interaction/motion) captured from the
  running artifact, never a mockup.

**Named prerequisite for phase 2 — the capture stack:** playwright +
chromium added to the agent image, plus a `tools/capture` wrapper (bun/TS,
per scripts-over-bash) that drives the tailnet preview URL and writes a
PNG/webm. Landing a browser in the agent image is a cross-record dependency
on the image-publishing lane (`publish-agent-image.yml`) — tracked as OQ6.
**Attach mechanism (chosen explicitly):** evidence is committed to a
dedicated orphan `pr-assets` branch and linked by stable
`raw.githubusercontent.com` per-commit URL — chosen over GitHub's
markdown-image CDN (a browser-session upload flow with no clean headless
API) and over a separate asset repo (an extra cross-repo write credential);
pruning merged-PR assets rides the phase-2 prerequisite work.

Skill content: lane decision tree, preview-driving steps (B2's URL shape),
the click-path template (phase 1), capture + attach mechanics (phase 2, once
`tools/capture` exists), e2e invocation (`go test -tags podman ./e2e/...`) +
live-mode env per Record A's creds surface. Register both in the
`~/.agents/AGENTS.md` on-demand indexes (`AGENTS.md:129-130` list
rules/skills by name). Delivery to the fleet rides DL-078 (Record A); this
task ends at the merged dotfiles PR.

Interfaces:

- Consumes: rule shape (`~/.agents/rules/planning-evidence.md:1-3`
  frontmatter precedent); skill shape (`~/.agents/skills/design/SKILL.md:1-4`);
  the `AGENTS.md` index lists (`~/.agents/AGENTS.md:129-130`); B2's link
  shape + B5's PR-body conventions.
- Produces: `personal/matt/agents/rules/pr-validation.md` (phase-1 form with
  the phase-2 escalation clause named and gated on the capture
  prerequisite), `personal/matt/agents/skills/compass-pr-validation/SKILL.md`,
  and the two index-line additions — one dotfiles PR.

Test cycle: red — no rule requires validation evidence, and PRs land
evidence-less today; green — the files exist, `~/.agents/` symlinks resolve
them (nix converge per Record A/DL-078), and a probe PR authored under the
rule carries the phase-1 evidence section (preview link + click-path).

## Tasks

- [ ] B1 [compass-ui] preview-parameterized UI build: `VITE_COMPASS_BASE_URL` (preview's door, `:50161`) / `VITE_COMPASS_TOKEN` (preview-minted bearer) → a `dist` that boots against preview's network door (red-green vs the devenv TLS door)
- [ ] B2 [platform] preview deploy workflow: `preview`-label single-holder lifecycle (claim/displace/release + displacement comment), full-stack deploy to `~/compass-envs/preview` + `compass-preview.service` restart, tailscale-serve'd UI at one stable tailnet URL
- [ ] B3 [harness] scenarios: multi-peer fan-out + provision/enroll hardening (double-run + `-race` green); PR-submitting scenario split out as a stub handed to the forge-stack lane (DL-052) with its four-layer gap named
- [ ] B4 [harness] scenarios: home-channel first-turn (delivery-cursor gate promoted to a primitive) + restart-replay barrier — lands after #256/#303 merge
- [ ] B5 [platform] result surfacing: `tools/e2e-summary` (go test -json → step-summary table), failure artifacts, sticky PR comment via a minimal-permission `workflow_run` workflow (PR number via artifact, treated as untrusted)
- [ ] B6 [agents-repo] `rules/pr-validation.md` (PHASED: preview link + click-path now; capture mandatory once the `tools/capture` stack ships) + `skills/compass-pr-validation/SKILL.md` in personal/matt/agents + AGENTS.md index lines (own dotfiles PR; fleet delivery per DL-078/Record A)

## Alternatives considered

**Lane 1 architecture — RULED by Matt (re-ruling DECIDED#8; formerly OQ1):
arm (b), the full PR stack on the `preview` env, with a deliberate
PR-selection mechanism** ("preview env but there needs to be some way of
choosing which PR goes to preview" — hence §A1's `preview`-label
single-holder). The grounding: industry practice splits PR previews by
backend-weight — static-frontend-only previews (the Vercel/Netlify model)
fit only frontend-heavy apps with minimal backend, while apps whose PRs
change backend behavior use a full-stack preview environment, so the
reviewer uses the ACTUAL feature rather than imagining it from the diff.
Compass is a Go gRPC server with a thin UI, and its dominant PR shape moves
proto+server+UI together — squarely the full-stack case.

- **(a) Static UI bundle on `main` — REJECTED.** The record's former
  designed floor: serve the PR's static `dist/` from `mattfw`, dialing
  `main`'s network door with a baked non-admin reviewer bearer. Wrong fit by
  the backend-weight heuristic: it previews only the pure-UI PR minority,
  and the dominant proto+server+UI PR gets no UI preview until its server
  half MERGES — after Matt's review, exactly when the preview was meant to
  help. It also carried an entire live-wave exposure class that arm (b)
  eliminates: a bearer baked into tailnet-readable static JS, the
  agent-kind-vs-user-kind authority analysis, CSP/egress hardening on the
  static host, and `main`'s single CORS slot spent on a preview origin.
- **(c) Per-PR ephemeral full stacks — the documented SCALE-UP path.** The
  industry gold standard (one environment per open PR, zero contention).
  Rejected for MVP as heaviest-on-one-box: N concurrent devenv stacks, each
  with its own postgres, on one shared host. If concurrent preview demand
  outgrows the one shared env, this is the recorded next step; the label
  mechanism's claim/release semantics port naturally.
- **PR-selection mechanisms considered** (Matt delegated the mechanism to
  the designer): a `/preview` ChatOps comment command — needs a
  bot/command parser, more infra; manual `workflow_dispatch` by PR number —
  works but not discoverable from the PR; auto-deploy-newest-across-all-PRs
  — REJECTED outright, the last-synchronize-wins serialization chaos Matt's
  requirement forbids. The `preview` label wins as the boring standard:
  visible on the PR, native `pull_request` events, one-click claim.

**Resolved with the ruling** (formerly OQ2/OQ3 — both existed only because
arm (a) pointed a bearer at the live wave):

- **OQ2 (reviewer bearer in the bundle) — DISSOLVED.** The baked bearer now
  authenticates a DISPOSABLE preview env with no `main` authority, so the
  tailnet-readable-token risk carries no live-wave consequence.
- **OQ3 (what the preview account may reach on the live wave) — DISSOLVED.**
  Under arm (b) preview has NO live work channels and no `RespondToAsk`
  reach into live agents — the forge-approval risk evaporates because the
  account lives on an isolated env.

## Open Questions

OQ1-OQ3 are folded above (OQ1 ruled by Matt; OQ2/OQ3 dissolved under the
ruling — see Alternatives considered). What remains are ONLY
non-load-bearing deferrals: each carries a recommendation the record designs
against, so no task blocks on an answer.

1. **OQ4 — Live-model e2e in the agent's pre-submit run** (non-load-bearing).
   A2.2 has agents run the suite pre-submit; deterministic mode is free, live
   mode spends LLM budget per run. Arms: (a) deterministic-only pre-submit,
   live-mode nightly on `preview` only — recommended, matches the harness
   record's D1 cadence ("live … on-demand/nightly, never per-PR",
   `compass-dogfood-e2e/design.md:105-113`); (b) live-mode pre-submit for
   PRs touching model/agent code. **Recommend (a)**, with (b) available
   on-demand when the diff is agent-lane.
2. **OQ5 — Rule scope: compass-only or every Matt repo** (non-load-bearing).
   The pr-validation rule could bind only compass PRs or all repos. Arms:
   (a) all repos, compass as the worked example — recommended: the failure
   mode (Matt pulling branches to see UI changes) is not compass-specific,
   and rules are global by construction (`~/.agents/AGENTS.md:3` "Global,
   always-on rules"). The rule's REQUIREMENT stays generic — a user-visible
   change ships running-artifact evidence appropriate to the repo — while the
   compass-specific mechanics (the preview link, the e2e scenario suite) live
   in the skill as the worked example, so the global rule is satisfiable on a
   non-compass PR; (b) compass-only, generalize later. **Recommend (a).**
3. **OQ6 — Capture-stack delivery: where do playwright/chromium +
   `tools/capture` live?** (non-load-bearing). B6's phase-2 evidence needs a
   capture stack that does not exist in the tree (no
   playwright/puppeteer/chromium anywhere; UI tests run under happy-dom,
   `apps/ui/test-setup.ts:1-4`). Arms: (a) in the agent image — uniform for
   every agent, but grows the image and makes B6 phase 2 a cross-record
   dependency on the image-publishing lane (`publish-agent-image.yml`);
   (b) per-box install on the wave hosts via nix (a Record A ops item) —
   smaller image blast radius, but only wave-resident agents can capture.
   **Recommend (a)**; either way B6 phase 1 ships now and phase 2 activates
   when this lands.

---

Ledger-impact: none — composes DECIDED#8 and frozen DL-026/DL-078 via Record
A; platform-corpus record, outside the design-ledger-gate's product scope
(`tools/design-ledger-gate/index.ts:45`).
