# Compass unified release lane (release-please + folded image publishing)

Status: Active

Linear:

- TBD (spawning agent stamps the issue at PR time)

> **Design record.** Citations name paths in `compass` = RigelBuild/compass
> (this repo, main at authoring, except where a citation names the unmerged
> PR #711 workspace). Line numbers drift as code evolves.
>
> **Supersedes (partially):**
> [`compass-release-bundling.md`](compass-release-bundling.md) — Fork 3
> (two-lane split, §192-235), Fork 4 (`v*`-tag controls, §275-315), OQ-3
> (binary-only trigger breadth, §481-491), and OQ-7's *bump mechanism* clause
> ("manual now, release-please at GA", §499-503). Every OTHER ruling of that
> record stays frozen and live — notably OQ-7's pre-GA `0.MINOR.PATCH` scheme,
> `v1.0.0` at GA, the ONE-whole-product-version attach-check architecture, and
> the post-1.0 MAJOR rule (§495-516). It also proposes retiring
> `.github/workflows/publish-agent-image.yml`, whose design record
> [`compass-agent-image-publish`](../infra/ci/compass-agent-image-publish/design.md)
> is compass-managed's zone — the zone owner has ACCEPTED the retire and will
> write their own superseded-by amendment (see §Cross-lane coordination).

## Problem / Intent

Matt ruled two changes to the frozen release-bundling design (2026-08-25):

1. **Cadence = release-please, NOW.** One whole-product semver, cut by a
   standing release-please "Release PR" that accumulates merged conventional
   commits; merging it (Matt's act) cuts `vX.Y.Z` with a batched
   conventional-commit CHANGELOG — the Claude-Code/OMP shape. This overrides
   OQ-7's "manual tag push now, automation at GA" staging and replaces the
   Fork 3(c) two-lane split (per-push `build-<sha12>` prerelease + manual `v*`
   semver) with ONE lane cutting real releases.
2. **A release builds/publishes the IMAGE, not a digest pointer.** `vX.Y.Z`
   must exist as a pullable `ghcr.io/rigelbuild/compass-agent:vX.Y.Z` tag
   alongside the four Go binaries — one version across the whole product.
   Realization: fold image publishing into the unified release lane and retire
   the separate `publish-agent-image.yml` FILE while relocating its DUTY.

This record designs that unified lane and the release-please integration for a
go+nix+bun monorepo, under the hard constraint that per-push `:git-<sha12>` +
`:latest` image publishing is PRESERVED (verified consumers below).

## Approach

### A1 — one workflow, three duties, two triggers

ONE workflow file (`.github/workflows/release.yml`, replacing both the PR #711
draft and `publish-agent-image.yml`) carries three jobs on TWO triggers —
never release-only, per the agent-image zone owner's steering:

- **`release-pr`** (trigger: every `push: branches: [main]`, unfiltered) —
  runs `googleapis/release-please-action` authenticated via a scoped GitHub
  App token (§Release-PR CI credential), creating/updating the standing
  Release PR. When the Release PR merges, the same action run on that merge
  commit creates the `vX.Y.Z` tag + GitHub Release and emits
  `releases_created: true` + `tag_name` as job outputs.
  Permissions (job-level): `contents: write` + `pull-requests: write`, nothing
  else.
- **`publish-image`** (trigger: the same `push: main`, self-gated by an
  in-job changed-paths check) — the RELOCATED per-push duty: builds the image
  spec and publishes `:git-<sha12>` + `:latest` via `agent-image/publish.sh`
  VERBATIM (no args = the default two-tag set, `publish.sh:48-56`), then runs
  the two-copy coherence verify copied from `publish-agent-image.yml:181-212`.
  Permissions: `contents: read` + `packages: write`. Concurrency group
  `publish-agent-image`, `queue: max` + `cancel-in-progress: false`.
- **`release-assets` + `release-image`** (gated on
  `needs.release-pr.outputs.releases_created == 'true'`) — build the four Go
  binaries stamped with the released version, upload assets to the
  release-please-created Release, and mint `:vX.Y.Z` on GHCR by digest re-tag
  (§A4). `release-assets` runs `contents: write` only; `release-image` runs
  `contents: read` + `packages: write` only.

*Why same-workflow output gating for the release jobs:* Releases and tags
created by the workflow token do NOT trigger other workflows (GitHub's
recursion guard), so an `on: push: tags: ['v*']` or `on: release` follow-up
workflow would silently never fire. Gating `release-assets`/`release-image` on
the release-please action's outputs inside the same run sidesteps that entirely
— no follow-up event, no credential needed for the DOWNSTREAM half. (The
UPSTREAM half — CI on the release-please-opened Release PR — is a separate
concern the recursion guard ALSO bites; it is solved by a scoped GitHub App
token, §Release-PR CI credential + §A5, not by this gating.)

*Why an in-job path gate for `publish-image`, not `on.push.paths`:* the
`release-pr` job must see EVERY main push (a paths filter would drop commits
from the changelog and stall the Release PR), and `on.push.paths` is
workflow-level. The per-push image job therefore self-gates with a push-event
before/after changed-path check over the closure set
`publish-agent-image.yml:49-64` names (`agent-image/**`,
`packages/compass-agent/**`, `package.json`, `bun.lock`, the workflow file
retargeted to `release.yml`, `tools/toolchain/versions/bun.nix`), defined ONCE
as a workflow-level `env` and shared with T3's resolver cross-check. This is a
git-diff over the push range, NOT ci.yml's moon-affected query (`ci.yml:1814`,
a related but distinct technique), and it MUST force-publish on the
`workflow_dispatch` path (dispatch has no push range to diff, and §A4's
remediation depends on dispatch always publishing).

### A2 — release-please strategy + the version source of truth

release-please runs `release-type: simple` (root component, no monorepo
manifest fan-out): the product versions as ONE unit — eight `go/cmd/*` binaries
carry an identical `var version = "0.1.0"` fallback stamped at build via
`-ldflags "-X main.version=<v>"` (e.g. `go/cmd/compass-stack/main.go:37-40`),
and `compass-stack` feeds it to `Deps.ExpectedVersion` (`main.go:310`) so
client/server are architecturally same-version (OQ-7's frozen clause, intact).

The bumped source of truth is a root **`version.txt`** (the `simple` strategy's
native target), plus `.release-please-manifest.json` +
`release-please-config.json`. Build-time flow of the ONE version:

- **Release builds** read it: `-X main.version=$(cat version.txt)` — the exact
  `ldflags` mechanism PR #711's build step already uses
  (`ws-release-rail/.github/workflows/release.yml:167`), with the clean
  `X.Y.Z` (no `+g<sha>` suffix; the release tag IS the identity).
- **The image tag** uses the same value: `:v$(cat version.txt)` at the release
  sha equals `tag_name`.
- **Dev/bundle builds** keep stamping locally (`app-bundle/build.sh`'s
  `<version>+g<shortsha>` shape, now reading `version.txt` as the base instead
  of the hardcoded `0.1.0`).
- The eight Go `var version` fallback constants are NOT bumped by
  release-please (kept out of the blast radius; a stale fallback is only ever
  visible in an unstamped `go run`). Annotating them with
  `x-release-please-version` generic-updater markers is a non-load-bearing
  option (OQ-N2).

### A3 — what a release contains

Salvaged from PR #711 (the superseded implementation — reference, not kept
as-is): the nix `langs`→PATH bootstrap, the CGO-free 4-binary build
(`compass`/`compass-server`/`compass-runner` linux-amd64 + `compass`
darwin-arm64, `-trimpath`), `SHA256SUMS`, and the release-notes generator
(`tools/release-notes/index.ts` — pure `assemble()` core, GHCR image identity,
nix-outputs manifest, bun:test). Dropped from #711: the `build-<sha12>`
prerelease identity, the create-or-update prerelease step, and the
image-recorded-absent degradation as a steady state (a semver release
hard-requires its `:vX.Y.Z` image; §A4). The release body = release-please's
conventional-commit CHANGELOG section + the generator's asset/image/nix-outputs
appendix (`gh release edit`-appended after upload).

### A4 — the `:vX.Y.Z` image: digest re-tag, never a second build

The image build is the heavy ~90m nix closure; the design MUST NOT build it
twice. The release sha is the Release PR's merge commit, which touches only
`version.txt` + `CHANGELOG.md` + manifest — none of which is in the image
closure path set — so the image content at the release sha is byte-identical
to the image already published for the last closure-affecting main sha.

**Chosen: re-tag the newest already-published image at-or-before the release
sha.** The `release-image` job (`fetch-depth: 0`):

1. Resolves the source tag by walking first-parent ancestors of the release
   sha, newest-first, and taking the first whose
   `ghcr.io/rigelbuild/compass-agent:git-<sha12>` resolves on GHCR (skopeo
   inspect). That tag's tree provably contains every closure change
   at-or-before it, so it is byte-identical to the release sha's image content
   — WITHOUT a second identity key. (A path-based `git log ... -- <paths>`
   resolver was rejected: the per-push lane keys its tag on the PUSH HEAD sha,
   not the last closure-touching commit, so a multi-commit push landing a
   closure change below its tip would make a path resolver assert a tag that
   was never published — a wedged release the dispatch remediation cannot
   satisfy.)
2. Hard-fails with a remediation pointer if no ancestor tag resolves within a
   bounded walk (workflow_dispatch the per-push publish on that sha, then
   re-run). A missing source image NEVER silently triggers a rebuild here.
3. `skopeo copy`s that tag's digest-resolved manifest to `:vX.Y.Z` — a cheap
   registry-side manifest write, no nix build at all.
4. Verifies `:vX.Y.Z` resolves and shares the source's config digest (the
   same verify shape as the two-copy check, `publish-agent-image.yml:204-211`).

The job runs inside the SAME `publish-agent-image` concurrency group as the
per-push job under `queue: max` (NOT the default single pending slot), so a
`:vX.Y.Z` mint serializes behind an in-flight `:latest` move WITHOUT being
cancelled by a later per-push entrant claiming the one pending slot — a
non-superseding release mint must never be silently dropped. `:latest` stays
owned and moved EXCLUSIVELY by the per-push job — the release lane never
touches it (the frozen Fork 3 rationale for dropping `latest` from the release
invocation, `compass-release-bundling.md:220-232`, carries over unchanged).

Rebuild-at-release (running `publish.sh git-<sha12> v<X.Y.Z>` on the release
sha) was weighed and rejected: it costs the full closure build for a
guaranteed-identical artifact, doubles the cache-miss surface, and — because
`publish.sh` builds before tagging — would put a second builder in a lane the
frozen record deliberately kept single-builder.

### A5 — controls on the new human act (Fork-4-equivalent)

Fork 4's hard controls guarded the old human act (pushing a `v*` tag). The
human act is now MERGING the Release PR; the controls re-express as:

1. **The `v*` tag ruleset stays, allowlist repointed.** Fork 4's GitHub tag
   ruleset restricting `v*` creation is kept but now permits ONLY the scoped
   GitHub App identity (release-please is the only tag minter) with repo-admin
   bypass for break-glass — so a leaked or over-scoped credential still cannot
   mint a release-triggering tag, and neither can a human by habit.
2. **Main-only by construction + guard.** The release jobs run only off the
   `push: main` event (`if: github.ref == 'refs/heads/main'`, the
   `publish-agent-image.yml:87` guard, kept on every job), and the tag
   release-please cuts names the Release PR's merge commit ON main — ancestry
   is by construction, not a post-hoc `merge-base` check.
3. **The merge gate IS the review gate.** The Release PR is an ordinary PR
   through branch protection: required checks + Matt's merge. Its CI runs
   because release-please opens it via a scoped GitHub App token (§Release-PR
   CI credential — a bare workflow-token PR gets no CI under the recursion
   guard); the token is installation-scoped and per-run, NOT a standing PAT.
4. **Releases serialize without dropping.** The image jobs share the
   `publish-agent-image` group under `queue: max` + `cancel-in-progress: false`
   (§A4), so a release cut is never half-superseded mid-upload nor silently
   cancelled by a later per-push entrant.

### A6 — security posture (copied verbatim where reused)

Wherever the unified lane reuses the frozen lanes' steps, the posture copies
VERBATIM:

- `cachix/install-nix-action` with the two caches named inline in
  `extra_nix_config` — NOT `accept-flake-config`
  (`publish-agent-image.yml:103-115` / #711 `release.yml:84-96` — the
  attacker-substituter rationale travels with the block).
- NO `pull_request` trigger, ever — fork-PR code never runs with a write
  token.
- The main-ref dispatch guard on every job (`publish-agent-image.yml:84-87`).
- `REGISTRY_AUTH_FILE` pinning + GHCR login via env-passed actor
  (`publish-agent-image.yml:150-173`).
- The skopeo bootstrap from `tools/toolchain/skopeo-nix2container-env.nix`
  (`publish-agent-image.yml:117-148`).
- Least privilege PER JOB: `packages: write` exists ONLY on the two image
  jobs; `contents: write` ONLY on the release-please + asset-upload jobs; the
  ci.yml gate keeps `contents: read`.

### Cross-lane coordination (resolved, accepted by owner)

`publish-agent-image.yml` and its record
`docs/designs/infra/ci/compass-agent-image-publish/` are compass-managed's
zone. The zone owner has ACCEPTED the fold-and-retire (steering relayed
2026-08-25): they review this record's PR and write their own superseded-by
amendment on their record pointing at this one as part of landing it. The
retire itself (deleting the workflow file) lands in the implementation PR with
their review; this record only carries the proposal + the acceptance.

### Hard constraint — per-push publishing preserved (the load-bearing fold rule)

"Retire the old lane" = retire the FILE, RELOCATE the DUTY. Per-push
`:git-<sha12>` + `:latest` publishing continues on every qualifying main push
because verified consumers break otherwise:

- `.github/workflows/ci.yml:1831-1832` — the dogfood e2e leg
  `podman pull ghcr.io/rigelbuild/compass-agent:latest` on
  non-image-affecting PRs (freshness consumer; `:latest` is Matt-ruled
  always-fresh, `ci.yml:1810-1812`).
- `app-bundle/SMOKE.md:59-62` — the native app's `compass-stack` pulls
  `:latest` from GHCR at first run (DL-112: the bundle ships NO image).
- `docs/designs/infra/ci/compass-agent-image-publish/design.md:33-43` —
  `:git-<short-sha>` is IMMUTABLE and is "the pin compass-stack bakes into the
  native app binary".

release-please only acts at release time, so the unified lane's `publish-image`
job is what keeps these consumers fed between releases.

## Alternatives considered

### Cadence (Matt ruled: release-please now)

- **release-please NOW (chosen, Matt 2026-08-25):** batched
  conventional-commit changelog, one human act (merge the Release PR), the
  bump derived from commit types the repo already writes. Brings OQ-7's
  planned GA end-state forward instead of building the interim twice.
- **Per-push auto-patch** (every main push bumps PATCH): release noise — a
  release per commit is the `build-<sha12>` prerelease lane wearing semver
  clothes; no batching, no human gate, and PATCH stops meaning anything.
- **The deferred two-lane split** (frozen Fork 3(c): `build-<sha12>`
  prerelease per push + manual `v*` semver): builds a prerelease rail that
  release-please obsoletes at GA anyway, and keeps the tag-push human act
  Fork 4 had to harden with a ruleset + ancestry guard. Superseded.

### `:vX.Y.Z` image realization

- **Digest re-tag of the per-push image (chosen, §A4):** one builder, cheap
  manifest copy, provable coherence with the `:git-<sha12>` pin.
- **Rebuild at the release sha:** ~90m closure rebuild for a byte-identical
  artifact; a second builder in a deliberately single-builder lane. Rejected.

### Workflow topology

- **One workflow, jobs gated on release-please outputs (chosen, §A1):**
  avoids the `GITHUB_TOKEN`-created-tag/release-doesn't-trigger-workflows trap
  for the DOWNSTREAM release jobs with no extra credential (the App token,
  below, is a separate concern — CI on the Release PR, not event propagation).
- **Two workflows (release-pr + on-release build):** would need the release
  EVENT to fire a second workflow; same-run output gating removes that need
  entirely, so this buys nothing. Rejected.
- **Fold into ci.yml:** rejected for the same reasons both frozen lanes
  document in their headers (least privilege, concurrency polarity, hot-path
  latency; `publish-agent-image.yml:3-28`).

### Release-PR CI credential (Matt ruled: GitHub App token, 2026-08-25)

The design-critic caught that a `GITHUB_TOKEN`-opened Release PR gets no
`pull_request` CI (GitHub's recursion guard), so branch protection cannot gate
it. Three options were put to Matt:

- **GitHub App installation token (chosen):** release-please authenticates
  with a scoped (contents + pull-requests), installation-scoped, per-run App
  token, so the Release PR receives CI and the merge gate is real. A standing
  App to provision (T6), but the token itself is short-lived and narrowly
  scoped — materially safer than the standing fine-grained PAT Fork 4 rejected.
- **Don't require CI on the Release PR (gate on merge alone):** the PR diff is
  machine-generated (`version.txt` + `CHANGELOG` + manifest) and its tree
  already passed full CI when the underlying commits merged. Credential-free,
  but rests the gate entirely on the human merge. Not chosen.
- **`actions: write` + `workflow_dispatch` ci.yml on the release branch:**
  keeps `GITHUB_TOKEN` but a dispatched run does not report as the PR's
  `pull_request` required checks, so it may not satisfy branch protection
  cleanly. Weakest; not chosen.

## Global Constraints

1. **ONE version.** A release stamps one `vX.Y.Z` from `version.txt` across
   all four released binaries (`-X main.version`) AND the image tag; the
   attach-check (`go/cmd/compass-stack/main.go:38-40,310`) enforces
   same-version client/server at runtime (OQ-7 clause, intact).
2. **PRESERVE per-push publishing.** `:git-<sha12>` + `:latest` publish on
   every closure-affecting main push, exactly as today. Consumers:
   `ci.yml:1831-1832` (dogfood `:latest` pull), `app-bundle/SMOKE.md:59-62`
   (first-run `:latest` pull, DL-112), the baked `:git-<sha>` binary pin
   (`compass-agent-image-publish/design.md:33-43`). Never release-only.
3. **The fold regresses NOTHING** (zone-owner conditions, all four carried):
   - (a) image publishes SERIALIZE — concurrency group `publish-agent-image`
     under `queue: max` + `cancel-in-progress: false`
     (`publish-agent-image.yml:76-78`, extended with `queue: max`); an
     in-flight `:latest` move is never half-superseded, and the non-superseding
     `:vX.Y.Z` release mint shares the group WITHOUT being cancelled by a later
     per-push entrant claiming the single default pending slot (bare
     `cancel-in-progress: false` alone would drop it);
   - (b) least privilege `contents: read` + `packages: write` on image jobs,
     NO `pull_request` trigger, main-ref guard
     (`publish-agent-image.yml:68-70,87`);
   - (c) `:git-<sha>` immutability = the config-digest refuse-overwrite guard
     (`publish.sh` `guard_immutable`) AND the two-copy `:git`/`:latest`
     config-digest coherence verify (`publish-agent-image.yml:181-212`) —
     both carried;
   - (d) off the hot path — no release/publish job is a required check; a
     publish flake never reds the merge gate.
4. **Reuse `agent-image/publish.sh` verbatim** — the positional-tag seam
   (`publish.sh:48-56`) is the only push mechanism; no new one.
5. **Security posture verbatim where copied:** caches named inline in
   `extra_nix_config` (never `accept-flake-config`); no `pull_request`
   trigger; main-ref guard; skopeo from the pinned helper; pinned
   `REGISTRY_AUTH_FILE`.
6. **Public image, public releases.** The image stays public (Matt-ruled);
   release assets stay anonymously downloadable (the config-publish consumer
   is anonymous, `compass-release-bundling.md:264-267`).
7. **Least privilege per job.** `packages: write` ONLY on `publish-image` +
   `release-image`; `contents: write` ONLY on `release-pr` +
   `release-assets`; ci.yml's gate stays `contents: read`.
8. **Pinned toolchains from nix, never `setup-go`** — the `langs` bootstrap
   (#711 `release.yml:98-117`) is the only PATH mechanism.

## Plan

### T1 — release-please configuration

Add `release-please-config.json` (`release-type: simple`, root component,
`include-component-in-tag: false` so tags are bare `vX.Y.Z`,
`bump-minor-pre-major: true` + `bump-patch-for-minor-pre-major: true` for the
OQ-N1 pre-1.0 damping Matt ruled), `.release-please-manifest.json` seeded at
the current `0.1.0`, and `version.txt` at `0.1.0`. Conventional-commit
subjects are already repo convention (rule://commit-conventions), so no
commit-style migration is needed.

- Interfaces: produces `version.txt` (consumed by T3's build step and
  `app-bundle/build.sh`), `release-please-config.json` +
  `.release-please-manifest.json` (consumed by T2's action).

### T2 — the unified workflow: `release-pr` job + per-push `publish-image` job

Rewrite `.github/workflows/release.yml` (superseding the PR #711 draft):
trigger `on: push: branches: [main]` (NO paths filter — release-pr must see
every commit) + `workflow_dispatch`; workflow-level `permissions: {}` with
per-job grants.

- `release-pr`: `googleapis/release-please-action` (SHA-pinned) authenticated
  via `actions/create-github-app-token` (App id + private key from secrets) —
  NOT `GITHUB_TOKEN` (§A5); job permissions `contents: write` +
  `pull-requests: write`; outputs `releases_created`, `tag_name`, `sha`.
- `publish-image`: job permissions `contents: read` + `packages: write`;
  concurrency group `publish-agent-image` under `queue: max` +
  `cancel-in-progress: false`; `if: github.ref == 'refs/heads/main'`; in-job
  changed-path gate over the closure set. The closure path set is defined
  ONCE (a workflow-level `env`) and consumed by BOTH this gate and T3's
  resolver cross-check, with the old self-reference
  (`publish-agent-image.yml`) retargeted to `release.yml`. NOTE the gate is a
  push-event before/after diff, NOT ci.yml's moon-affected query
  (`ci.yml:1814` — a related but distinct technique), and it MUST
  force-publish on the `workflow_dispatch` path (dispatch has no paths to
  diff; the §A4 remediation depends on dispatch always publishing). Other
  steps copied verbatim from `publish-agent-image.yml` (nix install w/ inline
  caches, skopeo bootstrap, auth-file pin, GHCR login, `./publish.sh` no-args,
  tag-resolve + coherence verify).
- Interfaces: consumes `agent-image/publish.sh` (unchanged),
  `tools/toolchain/skopeo-nix2container-env.nix`; produces GHCR
  `:git-<sha12>` + `:latest`.

### T3 — release jobs: binaries + assets, image re-tag

Same workflow, two jobs gated on
`needs.release-pr.outputs.releases_created == 'true'`:

- `release-assets` (`contents: write`): checkout at the release sha; nix
  `langs` bootstrap; build the 4-binary set with
  `-X main.version=$(cat version.txt)` and asset names
  `compass_vX.Y.Z_linux-amd64` etc. (#711's build step re-based from
  `build-<sha12>` to the semver tag); `SHA256SUMS`; run
  `tools/release-notes/index.ts` for the asset/image/nix-outputs appendix;
  `gh release upload "$TAG"` + append the appendix to the
  release-please-generated notes via `gh release edit --notes-file`.
- `release-image` (`contents: read` + `packages: write`; concurrency group
  `publish-agent-image` under `queue: max` + `cancel-in-progress: false`;
  `fetch-depth: 0`): resolve the source tag by walking first-parent ancestors
  newest-first and taking the first whose `:git-<sha12>` resolves on GHCR
  (§A4), hard-fail w/ remediation if none in a bounded walk, `skopeo copy` to
  `:vX.Y.Z`, verify config-digest equality with the source tag. Never touches
  `:latest`; never builds.
- Interfaces: consumes `version.txt`, `tools/release-notes/index.ts`
  (`assemble()` signature unchanged; `tag` input now `vX.Y.Z`),
  release-please outputs; produces GHCR `:vX.Y.Z`.

### T4 — retire `publish-agent-image.yml` (cross-lane, owner-accepted)

**Two-PR staging** (the same-PR precondition was self-contradictory: the new
workflow has no `pull_request` trigger, so `publish-image` cannot produce a
green per-push run inside the PR that would delete the old file). Instead:

1. Land the unified `release.yml` FIRST (T2/T3). The old
   `publish-agent-image.yml` stays live in parallel — this overlap is SAFE:
   both share the `publish-agent-image` concurrency group (groups span
   workflows repo-wide, so they serialize, never double-run) and `publish.sh`
   is idempotent on a matching digest (`guard_immutable`, `publish.sh:95-124`),
   so a redundant publish is a no-op, not a conflict.
2. Observe ONE green per-push `publish-image` run on a real closure-affecting
   main push (proves the relocated duty works end-to-end).
3. THEN delete `publish-agent-image.yml` in a follow-up PR, reviewed by
   compass-managed (the zone owner), who write the superseded-by amendment on
   their record.

- Interfaces: removes `.github/workflows/publish-agent-image.yml`; consumes
  compass-managed's PR review + their record amendment.

### T5 — tag ruleset repoint + local-stamp retarget + release-notes retarget

Update the Fork 4 `v*` tag ruleset allowlist to permit the App-token path
(repo-admin bypass retained — release-please is the only tag minter). Retarget
`app-bundle/build.sh:42`'s hardcoded `0.1.0+g<sha>` base to read `version.txt`
(the §A2 promise, previously unowned by any task — after the first real bump a
hardcoded base would silently report stale, and the SMOKE version-containment
assert (`build.sh:98-99`) would keep passing against the wrong base). Adjust
`tools/release-notes/index.ts` edge for the semver tag shape and the
required-image posture (a release with no resolvable image is a FAILURE, not a
degradation — flip the null-image arm's use at release time; the pure
`assemble()` core keeps the nullable type for the dry-run path). Update
`docs/architecture/build-and-ci.md`'s lane description.

- Interfaces: GitHub ruleset (settings surface, Matt applies — no IaC rail
  for repo rulesets today, OQ-N3); `app-bundle/build.sh` (base version from
  `version.txt`); `tools/release-notes/index.ts` (`classifyImageResult` +
  `main()` edge).

### T6 — provision the scoped GitHub App (release-please identity)

Create (or reuse an org) GitHub App with `contents: write` +
`pull_requests: write` on this repo, install it, and store its app-id +
private key as Actions secrets consumed by T2's `release-pr` via
`actions/create-github-app-token`. Settings/secret surface today (OQ-N3);
a prerequisite for `release-pr` to open a CI-gated Release PR.

- Interfaces: produces the App-token secrets consumed by T2; no code artifact.

### Ledger delta (applied same-PR by the spawning agent)

In `docs/designs/DECISIONS.md` (§Infrastructure & CI), append (next-free id
verified at PR time = DL-298; main advanced through 283-297 while drafting):

- **DL-298:** One whole-product release lane driven by release-please
  (`release-type: simple`, root `version.txt` source of truth): a standing
  Release PR batches conventional commits; merging it (Matt's act) cuts
  `vX.Y.Z` + CHANGELOG, builds the 4-binary asset set stamped from
  `version.txt`, and mints the `:vX.Y.Z` image tag — superseding the
  release-bundling record's Fork 3 two-lane split, Fork 4 `v*`-controls, OQ-3
  binary-only trigger, and OQ-7's manual-now/automation-at-GA staging (its
  other clauses stay Active) →
  `platform/compass-unified-release-lane.md`.
- **DL-299:** The `:vX.Y.Z` agent image is minted by digest re-tag
  (`skopeo copy`) of the already-published per-push `:git-<sha12>` artifact —
  never a second closure build; `:latest` stays owned exclusively by the
  per-push publish job; the mint shares the `publish-agent-image` concurrency
  group → `platform/compass-unified-release-lane.md`.
- **DL-300:** `publish-agent-image.yml` retires as a FILE with its DUTY
  relocated into the unified lane's per-push `publish-image` job (verbatim
  `publish.sh`, same paths gate, same serialize/immutability/coherence/
  least-privilege/off-hot-path posture) — accepted by the agent-image zone
  owner, who amend their record with a superseded-by pointer →
  `platform/compass-unified-release-lane.md`.
- **DL-301:** release-please authenticates with a scoped GitHub App
  installation token (contents + pull-requests, installation-scoped, per-run)
  so the Release PR receives `pull_request` CI and branch protection is a real
  merge gate — a bare `GITHUB_TOKEN`-opened PR gets none under the recursion
  guard (Matt-ruled 2026-08-25) → `platform/compass-unified-release-lane.md`.

Flips (the bundling record's decisions live in its prose, not as existing DL
rows — none found for Fork 3/OQ-3/OQ-7 in the ledger at authoring — so the
flip lands as record-header edits, not row-status flips):

- `docs/designs/platform/compass-release-bundling.md` `Status:` line gains:
  "Fork 3, Fork 4, OQ-3, and OQ-7's bump-mechanism clause superseded by
  `compass-unified-release-lane.md` (Matt, 2026-08-25); all other rulings
  remain Active."
- `docs/designs/infra/ci/compass-agent-image-publish/design.md` `Status:`
  amendment is written by compass-managed (their zone, accepted).

## Tasks

- [ ] T1 — release-please config + manifest + `version.txt` seed
- [ ] T2 — unified `release.yml`: `release-pr` job + per-push `publish-image`
      job (posture verbatim)
- [ ] T3 — release-gated `release-assets` + `release-image` (digest re-tag)
      jobs
- [ ] T4 — retire `publish-agent-image.yml` (two-PR staging; compass-managed
      review + amendment)
- [ ] T5 — `v*` ruleset repoint + `app-bundle/build.sh` version.txt retarget
      + release-notes retarget + build-and-ci.md update
- [ ] T6 — provision the scoped GitHub App + store credentials as Actions
      secrets (prerequisite for T2's `release-pr`; OQ-N3)
- [ ] Ledger delta: DL-298/299/300/301 + bundling-record `Status:` amendment
      (spawning agent, same PR)

## Open Questions

- **Release-PR CI credential — DECIDED (Matt, 2026-08-25): GitHub App
  installation token.** A `GITHUB_TOKEN`-opened Release PR gets no
  `pull_request` CI (recursion guard), hollowing the merge gate; release-please
  authenticates with a scoped App token instead so CI fires and branch
  protection gates the merge (§A1, §A5(3), DL-301, T6).
- **OQ-N1 — DECIDED (Matt, 2026-08-30, RIG-2997): both `feat:` and `fix:` bump
  PATCH; a MINOR is a deliberate manual bump.** Under the intact `0.MINOR.PATCH`
  scheme (OQ-7), `bump-patch-for-minor-pre-major` damps both `feat:` and `fix:`
  to PATCH while 0.x — too many features land per release to cut a MINOR for
  each. A deliberate MINOR is a manual `release-as: 0.N.0` in a separate PR; a
  breaking-change footer still auto-bumps MINOR (not 1.0.0) via
  `bump-minor-pre-major` while 0.x; `v1.0.0` is a manual `release-as: 1.0.0` at
  GA. Configured in T1 (both flags set).
- **OQ-N2 (non-load-bearing): stamp the eight `var version` fallbacks?**
  Whether release-please's generic updater should also bump the
  `var version = "0.1.0"` constants across `go/cmd/*` (via
  `x-release-please-version` annotations) so unstamped `go run` builds report
  the current version. Cosmetic; recommendation: no (keep the blast radius at
  `version.txt`), revisit if a stale fallback ever confuses a triage.
- **OQ-N3 (non-load-bearing, process): manual settings surfaces (tag ruleset +
  GitHub App).** Repo rulesets and the release-please GitHub App have no IaC
  rail in this repo today; T5/T6 hand Matt the exact ruleset edit and App
  provisioning as runbook steps (rule://no-human-clicks escape hatch). Accept,
  or scope a follow-up to bring both under Pulumi's github provider?
- **OQ-N4 (non-load-bearing): release cadence floor/ceiling.** ASSUMED:
  merge-when-Matt-chooses — the Release PR accumulates until Matt merges it,
  releases happen exactly then. A cadence floor/ceiling (auto-merge after N
  days, or a staleness reminder) is an additive follow-up, NOT part of this
  lane's contract — the mechanism is identical under any answer, so this defers
  cleanly and the merge ratifies the assumption.
- **Cross-lane retire — RESOLVED, accepted by owner.** compass-managed keep
  their record and write the superseded-by amendment themselves while
  reviewing/landing this PR (steering 2026-08-25). Recorded here for the
  review trail, not as an open fork.
