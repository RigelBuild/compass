# Bundle nix outputs, images, and binaries as GitHub Releases

> **Design record.** Citations name paths in `compass` = RigelBuild/compass at
> `c25ce94f` (this repo, main at authoring). Line numbers drift as code evolves;
> resolve against that revision. This is a public repo: the internal CD monorepo
> that consumes these artifacts is referred to by role, never by name, path, PR,
> or quoted source.

Status: Active — the three load-bearing forks (OQ-1/2/3) were ruled by Matt
(2026-08-22); freezes on merge as the contract T1–T5 execute against.
Tracking: RIG-1746 (whole-product semver + GitHub Releases). RIG-2103 is
retargeted onto T4 (OQ-2); the OQ-1 desktop deferral is RIG-2477.

## Problem / Intent

CI/CD publishes container images to GHCR and an internal config-publish job
`go install`s a pinned compass CLI from source on every run — but nothing
produces a durable, versioned bundle of what a build actually shipped. This
record designs a GitHub Releases lane: one Release per qualifying build,
carrying the compiled binaries (compass CLI per-arch), the container image
identity (GHCR ref + digest), and the nix build output identity (store paths +
hashes), so consumers pin and download a verified artifact instead of
rebuilding from source.

**The record lives in the compass repo** because Fork 1 below concludes compass
owns the surface: GitHub Releases attach to a GitHub repo, and the public
`RigelBuild/compass` repo is where the release artifacts' sources live, where
the existing GHA publish lane runs, and where an in-repo `GITHUB_TOKEN` can
create Releases with zero new credentials. The internal CD monorepo is
Woodpecker-driven and would need a cross-repo PAT to write Releases anywhere
(§Fork 1).

## Approach

Four design forks, each with options, a recommendation, and tradeoffs.

### Fork 1: which repo + which surface owns Release creation

**Options:**

- **(a) compass repo, GitHub Actions** — a new `.github/workflows/release.yml`
  beside the existing publish lane.
- **(b) the internal CD monorepo, Woodpecker CD** — a push→main CD job like its
  image-publish lane, writing Releases on some GitHub repo via a PAT.
- **(c) hybrid** — compass GHA creates the Release; the internal CD appends its
  own assets to it.

**Recommendation: (a) compass GHA.** Grounds:

1. *The artifacts are compass-built.* The compass CLI is
   `go/cmd/compass` in the compass Go module — after the RIG-1930/RIG-2025
   module-path rename (compass PR #458, in review at authoring; a Global
   Constraint below), `compass go/go.mod:13` on that branch reads:

   ```text
   module github.com/RigelBuild/compass/go
   ```

   (main at `c25ce94f` still carries the pre-rename
   `github.com/sealedsecurity/compass/go`; the remaining in-flight half is the
   internal config-publish job's install target, still on the old path — §Fork 2).
   The agent image builds from
   `compass agent-image/` via the vendored devenv/nix2container forks —
   `compass agent-image/publish.sh:51`:

   ```text
   BUILD_OUT="$(nix run path:../forks/devenv#devenv -- container build agent)"
   ```

   A Release must attach to the repo whose commits it versions; releasing
   compass artifacts on the internal monorepo would version them against the
   wrong history.

2. *The GHA precedent already exists and is the right shape.* The compass repo
   deliberately runs artifact publishing as a separate least-privilege GHA
   workflow, not a CI-gate step — `compass
   .github/workflows/publish-agent-image.yml:3-9`:

   ```text
   # WHY A SEPARATE WORKFLOW, NOT A STEP IN THE CI GATE — a deliberate, principled
   # exception to this repo's ONE-JOB doctrine (see ci.yml's header):
   #
   #   - Least privilege. A GHCR push needs `packages: write`; the gate job runs
   #     `contents: read` only.
   ```

   A Release workflow is the same pattern with `contents: write` instead of
   `packages: write`.

3. *Auth is free in-repo.* GHA's ephemeral `GITHUB_TOKEN` can create Releases
   and upload assets in its own repo with a `permissions: contents: write`
   grant — no standing secret. The internal Woodpecker CD authenticates GHCR
   pushes with a provisioned `GHCR_TOKEN` — a packages-scoped credential;
   writing Releases from Woodpecker would need
   a *new* repo-scoped PAT to provision, rotate, and fork-gate (§Fork 4).

4. *The public repo is the consumption point.* `RigelBuild/compass` is public,
   so Release assets download anonymously — which is exactly what the internal
   config-publish consumer needs (§Fork 3). The internal monorepo is not the
   public surface.

**Tradeoffs accepted:** (b) would co-locate the Release cut with the internal
CD's other push→main GHCR legs, keeping one CD brain; but none of those legs
produce compass artifacts, and the cross-repo PAT + Woodpecker↔GitHub seam
outweighs the co-location. (c) hybrid is
deferred as non-load-bearing: today no internally-built artifact belongs in a
compass Release (the internal CD's images are CI step images, not product
artifacts). If that changes, the internal CD can append assets to an existing
Release with a narrowly-scoped PAT without reopening this record.

### Fork 2: what artifacts, and how each is produced

Three artifact classes. The principle: **attach what consumers download;
reference what registries already serve durably.**

**(i) Binaries — attach, per-arch.**

- *Which:* `compass` (the CLI — the one binary with a known external consumer:
  the internal config-publish job pins it, §Fork 3), plus `compass-server` and
  `compass-runner` (the deployable daemons —
  `compass go/cmd/` also carries `compass-stack`, `compass-app`,
  `compass-gen-cert`, `compass-mint-runner-token`, `compass-postgres`; those
  are dev/desktop tooling built on demand and are *excluded* from v1 to keep
  the asset set meaning something — an OQ records the boundary).
- *Arch matrix:* `linux-amd64` for all three binaries (the dogfood/CD target —
  the agent-image record froze linux/amd64 as the dogfood platform, `compass
  docs/designs/platform/compass-agent-image-publish.md:54-67`), plus
  `darwin-arm64` for the **CLI only** (Matt's macOS dev machines run the CLI
  against remote/dogfood stacks; the daemons deploy on Linux, and shipping
  darwin daemon builds would assert a support surface nothing consumes).
  Go cross-compiles the darwin CLI from the same ubuntu runner
  (`GOOS=darwin GOARCH=arm64 CGO_ENABLED=0`; the CLI is pure-Go — CGO is
  needed only by the pgtest suites, `compass .github/workflows/ci.yml:263`
  sets `CGO_ENABLED: '1'` for tests, not for the CLI build). Asset set:
  3 × linux-amd64 + 1 × darwin-arm64 = 4 binaries.
- *How:* plain `go build` in the release workflow, named
  `compass_<version>_<os>-<arch>` + a single `SHA256SUMS` asset. No goreleaser:
  the repo's convention is zero-extra-tooling glue (`compass
  agent-image/publish.sh:5-11` argues exactly this for bash-over-frameworks),
  and goreleaser's tag-driven model fights the per-build lane in Fork 3.

**(ii) Container image — reference by digest, do NOT attach a tarball.**

The agent image is already published durably to GHCR with an immutable
per-commit tag — `compass .github/workflows/publish-agent-image.yml:33`:

```text
podman pull ghcr.io/rigelbuild/compass-agent:git-<sha12>
```

and the publish enforces tag immutability + digest coherence
(`publish-agent-image.yml:143-178`, the verify step). A `docker save`/`skopeo
copy dir:` tarball would duplicate a multi-GB closure GitHub caps and GHCR
already serves, and would create a *second* image identity to keep coherent.
Instead the Release **body** records `ghcr.io/rigelbuild/compass-agent@sha256:…`
(the config digest the publish verify step already computes,
`publish-agent-image.yml:173`) — a durable pointer to an immutable artifact.
*Tradeoff:* if GHCR retention ever deletes the package, the Release's image
pointer dangles; accepted because the package is public + repo-linked and
nothing in the fleet deletes it, and a tarball's cost is paid on every release
for a disaster that has a rebuild path (`publish.sh` re-mints any sha via
`workflow_dispatch`, `publish-agent-image.yml:69`).

**(iii) Nix build outputs — attach the identity manifest, not the closure.**

The repo's nix outputs are the agent-image nix2container spec and the
toolchain derivations (`compass devenv.nix:29-36` — go from go-overlay,
bun/node/moon vendored as derivations). A full closure export
(`nix-store --export` / `nix copy` to a file) of the agent image is the same
multi-GB payload rejected in (ii), and CI already declares binary caches as
the closure-serving mechanism (`compass .github/workflows/ci.yml:165-168`,
the pinned substituters). **Attach instead a `nix-outputs.json` manifest**:
per built derivation, the store path + narHash from
`nix path-info --json` (the image spec path `publish.sh:52` captures, plus the
toolchain `langs` set `ci.yml:183-184` already evaluates). That makes every
Release a verifiable statement of *which* nix outputs the build produced —
reproducible byte-for-byte from the tagged commit — without shipping the
bytes twice. *Tradeoff:* a consumer without nix or cache access cannot
materialize the closure from the Release alone; accepted — no such consumer
exists (the runtime consumer pulls the GHCR image; developers have nix), and
an OQ records closure-attachment as a possible later add for offline/archival
use.

### Fork 3: versioning + trigger

**Options:**

- **(a) Release per push→main**, tagged by commit (`build-<sha12>` or
  CalVer+sha).
- **(b) Release per semver tag push** (`vX.Y.Z`), manual cut.
- **(c) Two lanes:** an automatic **prerelease** per qualifying main push
  (tag `build-<sha12>`, `prerelease: true`) + a **semver release** on `v*` tag
  push (RIG-1746's whole-product semver), both from the same workflow.

**Recommendation: (c).** The directive asks for "a durable, versioned Release
per build" — that is lane one. But RIG-1746 already reserves whole-product
semver as its own deferred concern (RIG-1746: "Whole-product semantic
versioning + GitHub Releases is DEFERRED to its own product-versioning
record"), and the agent-image record explicitly left the tag/release trigger
open for it (`compass docs/designs/platform/compass-agent-image-publish.md:
185-186`: "No tag/release trigger yet; that arrives with the GA
release-version tag"). Lane two implements that reserved slot: the same
workflow, triggered on `push: tags: ['v*']`, cuts a non-prerelease Release and
additionally pushes the `:vX.Y.Z` image tag via the positional-args seam the
agent-image record forward-planned (`publish.sh:38-39`: "A GA add is one more
positional arg (`./publish.sh git-<sha> v<semver> latest`)") — **invoked as
`./publish.sh git-<sha12> v<semver>`, deliberately WITHOUT `latest`.**
`publish-agent-image.yml` serializes every `:latest` move under its own
concurrency group (`publish-agent-image.yml:76-82`: "Publishes SERIALIZE — an
in-flight `:latest` move must never be half-superseded") and hard-fails its
verify when `:git-<sha>`/`:latest` digests diverge
(`publish-agent-image.yml:171-178`). A release-lane invocation moving `:latest`
under a *different* concurrency group (`release`) would break that
serialization: two workflows could interleave `:latest` moves, and the image
lane's verify would red on a divergence the release lane caused. Alternatives
weighed: *(a) share one concurrency group across both workflows* — GitHub
concurrency groups do span workflows, but that serializes every release cut
behind every image build (and vice versa) and couples the two lanes' latencies
for no gain; *(b) drop `latest` from the release invocation* — `:latest` is the
per-push image lane's pointer and a semver cut needs only `:vX.Y.Z`; *(c)
`workflow_dispatch` the image workflow to add the tag* — indirection plus a
second run for one manifest write. **(b) chosen**: the semver lane mints
`:vX.Y.Z` only; `:latest` stays owned, moved, and verified exclusively by
`publish-agent-image.yml`. What `vX.Y.Z` *means* across the product (cadence,
changelog, what bumps major) stays RIG-1746's record — this lane only gives it
the mechanical rail.

- *Per-build tag scheme:* `build-<sha12>` (12-hex short sha, matching the image
  tag derivation `publish.sh:43`). Git-sha, not date: it is the identity every
  existing pin already speaks (the pinned-rev constant, the `:git-<sha12>` image
  tag), and it is collision-free where CalVer needs a disambiguator.
- *Qualifying push:* a `paths:` filter over the binary-affecting inputs only —
  `go/**`, `tools/toolchain/versions/go.nix`, and the workflow file — NOT
  unioned with the image lane's closure paths (OQ-3, ruled). It reuses the
  `on.push.paths` native-scoping technique the image publish uses
  (`publish-agent-image.yml:44-68`) but not that lane's path *set*: an
  image-only sha (`agent-image/**`, `forks/**`, `bun.lock`) republishes the
  image with its GHCR per-sha identity but mints no Release, and a docs-only
  push mints nothing. Unioning the two path sets would mint binary-identical
  Releases on image-only shas — the exact outcome OQ-3 rejects.
  Prereleases are **published, not draft**: a draft is invisible to the
  anonymous asset-download consumer, and the per-build lane has no human step.
- *Semver releases:* draft-then-publish is unnecessary too — the tag push *is*
  the human act (only Matt pushes `v*` tags). That is a convention, not an
  enforcement, so Fork 4 adds two hard controls on the tag lane.
- *Consumer cutover (the RIG-2103 overlap):* the internal config-publish job
  currently provisions the CLI by compiling it (`go install` of the pinned rev),
  which requires a Go toolchain in the CD step image (the exact RIG-2025
  regression: the job runs in `ci-base`, which has none) and go-get module
  resolution (the exact module-path regression). **Ordering is explicit:** the
  in-flight RIG-2025 fixes (route the job to `ci-go`; rename the module path,
  compass PR #458 + the internal job's install-target repoint) land NOW and
  are independent of this record — T4 is a LATER revert of that routing once a
  Release exists to download, never a substitute for the P0 fix. With this
  lane, the job instead downloads
  `https://github.com/RigelBuild/compass/releases/download/build-<sha12>/compass_<…>_linux-amd64`
  and verifies against `SHA256SUMS` — anonymous (public repo), toolchain-free,
  and still pinned by the same pinned-rev constant. No Release covers the current
  pin `a61d0caf` retroactively, so T4's landing path is a **pin advance**: move
  the pinned-rev constant to the first released sha as its own reviewed change,
  then cut the sourcing over. RIG-2103 proposed a nix derivation for the same
  problem; the Release asset is the lighter answer and T4 below proposes
  retargeting RIG-2103 to this cutover rather than running both. Matt ruled
  RETARGET (2026-08-22, OQ-2); RIG-2103's Linear scope was updated to match.

### Fork 4: auth + fork-safety

**Options:** (a) the workflow's own `GITHUB_TOKEN` with `contents: write`;
(b) a releases-scoped fine-grained PAT; (c) reuse/widen the internal CD's `GHCR_TOKEN`.

**Recommendation: (a).** Same-repo Release creation is exactly what
`GITHUB_TOKEN` covers; the agent-image record already weighed and rejected a
PAT for the analogous GHCR case (`compass
docs/designs/platform/compass-agent-image-publish.md:245-247`: "a PAT/deploy
token — needed only for cross-repo or cross-org pushes, which this is not; a
standing secret to rotate for zero benefit"). (c) is a category error:
`GHCR_TOKEN` is the internal Woodpecker CD's packages credential, provisioned
for image pushes — it never touches this GHA surface, and widening it to
`contents: write` would raise the blast radius of the internal CD's
most-injected secret for nothing.

**Fork-safety:** the workflow triggers only on `push: branches: [main]`,
`push: tags: ['v*']`, and `workflow_dispatch` — no `pull_request` event, so
fork-PR code never executes with the write token (the identical posture to
`publish-agent-image.yml:44-69`, which has no PR trigger for the same reason).
The dispatch lane carries the same main-ref guard the image publish uses
(`publish-agent-image.yml:88-91`: "guard so a dispatch from a feature branch
can never mint a `:git-<sha>` for unmerged code"). Workflow-level
`permissions:` block grants `contents: write` and nothing else; the CI gate
keeps its `contents: read` (`compass .github/workflows/ci.yml:90-91`).

**Tag-lane hardening (the `v*` injection hole).** A `push: tags: ['v*']`
trigger fires at whatever commit the tag names, wherever it lives — "agents
never push tags" (Global Constraint 5) is a convention, not an enforcement,
and the push-guard gates agent pushes, not every credential that could mint a
tag. Two hard controls close it:

1. **A GitHub tag ruleset** on `RigelBuild/compass` restricting `v*` tag
   creation to Matt (repo-admin bypass only) — the platform-level control, so
   a leaked or over-scoped credential cannot create a release-triggering tag.
2. **An in-workflow ancestry guard** on the tag lane: fail unless the tagged
   commit is on main —
   `git merge-base --is-ancestor "$GITHUB_SHA" origin/main` — mirroring the
   image workflow's dispatch main-ref guard (`publish-agent-image.yml:88-91`),
   so even a ruleset mis-configuration cannot cut a non-prerelease Release
   (and a `:vX.Y.Z` image tag) from unmerged code.

## Alternatives considered

- **The internal Woodpecker CD as the release surface** — rejected in Fork 1: wrong
  repo for the versioned history, needs a new cross-repo PAT, and the public
  download point is compass.
- **Image tarball as a Release asset** — rejected in Fork 2(ii): duplicates a
  registry-served multi-GB artifact and creates a second identity to keep
  coherent; the digest pointer is the durable record.
- **Full nix closure export as an asset** — rejected in Fork 2(iii): same size
  argument; the narHash manifest gives verifiability, caches give
  materialization.
- **goreleaser** — rejected in Fork 2(i): tag-only model fights the per-build
  prerelease lane; the repo convention is dependency-free glue scripts.
- **Semver-only releases (no per-build lane)** — rejected in Fork 3: fails the
  directive's "Release per build" and leaves the config-publish consumer with
  no per-rev asset to pin.
- **Signed provenance for v1 (attestations / cosign)** — weighed, deferred.
  `SHA256SUMS` uploaded from the same origin as the binaries is
  transfer-integrity, not provenance: a compromised workflow forges both.
  GitHub artifact attestations (`permissions: attestations: write` +
  `gh attestation verify`) or cosign signing would bind assets to the building
  workflow identity. Rejected for v1 because the only consumer is our own
  sha-pinned CD (T4), which already trusts the repo it pins; recorded as
  OQ-6 so a future external consumer triggers the add, not a redesign.

## Global Constraints

1. **Module path rename is a prerequisite.** The compass-side rename
   `github.com/sealedsecurity/compass/go` → `github.com/RigelBuild/compass/go`
   (RIG-1930 / RIG-2025) lands via compass PR #458 (in review at authoring);
   the internal job's install-target repoint is its in-flight other half. Every
   task here lands *after* the rename and uses the `RigelBuild` path
   exclusively; nothing in this design may reintroduce a
   `sealedsecurity/compass` reference.
2. **Release names are `build-<sha12>` (prerelease) and `vX.Y.Z` (release)**;
   binary assets are `compass_<tag>_<os>-<arch>` — three `linux-amd64`
   binaries + one `darwin-arm64` CLI (Fork 2(i)) — plus one `SHA256SUMS` per
   Release.
3. **No PR-event trigger, ever**, on any workflow holding `contents: write` —
   the E1 fork posture of every publish lane in both repos.
4. **The tag pair ordering rule extends:** immutable identities first
   (Release + `:git-<sha>`), moving pointers last (`:vX.Y.Z` image tag on the
   semver lane) — per `publish.sh:37-39`. `:latest` is exclusively the image
   lane's pointer; the release lane never moves it (Fork 3).
5. **`v*` tag creation is enforced, not assumed.** A GitHub tag ruleset
   restricts `v*` creation to Matt, and the tag lane hard-fails on a
   non-main-ancestor sha (Fork 4's two controls). "Agents never push tags"
   remains the convention those controls back. The per-build lane needs no
   tag-push actor (GHA creates the tag via the Release API at the built sha).
6. **Go version floor** tracks `tools/toolchain/versions/go.nix` (the same
   pin CI resolves, `ci.yml:170-195`); the release build must use the pinned
   toolchain, not a `setup-go` drift.

## Plan

**T1 — Release workflow: per-build prerelease lane.**
New `compass .github/workflows/release.yml`: trigger `push: branches: [main]`
with a `paths:` filter over `go/**`, `tools/toolchain/versions/go.nix`, and the
workflow itself; `permissions: {contents: write}`; concurrency group
`release`, `cancel-in-progress: false` (serialize, like
`publish-agent-image.yml:80-82`). Steps: install nix (the pinned
`cachix/install-nix-action` + substituter block copied from `ci.yml:150-168`),
resolve the pinned Go toolchain, build the four binaries (three ×
`linux-amd64` + the CLI × `darwin-arm64`, `CGO_ENABLED=0` for the darwin
cross-build), generate `SHA256SUMS`, `gh release create build-<sha12>
--prerelease` with the assets and a generated body carrying the image digest +
nix-outputs manifest (T2).
*Interfaces:* consumes HEAD sha + `go/cmd/{compass,compass-server,compass-runner}`;
produces Release `build-<sha12>` with 6 assets (4 binaries + `SHA256SUMS` +
`nix-outputs.json`). Acceptance: a main push touching `go/**` yields a
downloadable, checksum-verifying `compass_build-<sha12>_linux-amd64`.

**T2 — Release body + nix-outputs manifest generator.**
A small script (bash or bun, matching `publish.sh` conventions) that emits:
(a) the GHCR image ref + config digest for the sha (querying the tag with the
fork skopeo exactly as `publish-agent-image.yml:160-173` does; "image not yet
published for this sha" degrades to a recorded absence, not a failure — the
image lane is paths-filtered independently); (b) `nix-outputs.json` from
`nix path-info --json` over the toolchain `langs` set and, when built, the
agent-image spec path.
*Interfaces:* consumes `git rev-parse HEAD`, GHCR (read, anonymous),
`tools/toolchain/gate-tools.nix`; produces the release body markdown +
`nix-outputs.json`. Acceptance: unit-testable pure formatting; a dry-run mode
prints both without any GitHub write.

**T3 — Semver lane + image release-tag.**
Extend `release.yml` with `push: tags: ['v*']`: same build steps, Release is
non-prerelease named `vX.Y.Z`, guarded by the Fork 4 ancestry check
(`git merge-base --is-ancestor "$GITHUB_SHA" origin/main` — fail otherwise),
and additionally invokes `./publish.sh git-<sha12> v<semver>` (the
forward-planned positional add, `publish.sh:38-39`; deliberately WITHOUT
`latest` — Fork 3's concurrency resolution) so the GHCR image gains the
matching `:vX.Y.Z` tag. Requires `packages: write` on this lane only
(job-level permissions split). Includes provisioning the `v*` tag ruleset
(Fork 4 control 1) — an IaC change if the github Pulumi stack manages this
repo, else a documented one-time setting.
*Interfaces:* consumes the pushed tag ref; produces Release `vX.Y.Z` + GHCR
`:vX.Y.Z`. Acceptance: pushing a `v0.0.1-test` tag on a throwaway ref cuts a
coherent Release + image tag (then deleted); a tag on a non-main sha fails the
ancestry guard; documented in the workflow header.

**T4 — Consumer cutover: the internal config-publish job downloads the Release asset.**
Sequenced strictly AFTER the in-flight RIG-2025 fixes (ci-go routing + module
rename), which land now and independently — T4 is the later revert of that
routing, not its substitute. In the internal CD monorepo, two reviewed steps:
(1) **pin advance** — move the pinned-rev constant to the first released sha (no
Release covers the current pin `a61d0caf` retroactively); (2) replace the
`go install` provisioning with a download-and-verify of
`releases/download/build-<sha12>/compass_…_linux-amd64` + `SHA256SUMS` check,
then drop the Go-toolchain requirement from the CD job (reverting the RIG-2025
ci-go routing). Keep the single pinned-rev constant. RIG-2103 is retargeted onto
this task (Matt ruled RETARGET, 2026-08-22, OQ-2); the exact file targets live
in RIG-2103.
*Interfaces:* consumes Release assets (anonymous HTTPS); produces the same
`binDir/compass` provisioning contract the CD job returns today. Acceptance:
the existing unit tests' injected-runner seam re-covers the new argv; a CD run
on main provisions the CLI with no `go` in the image.

**T5 — Docs + record freeze.**
Update `docs/architecture/build-and-ci.md` (the doc the image publish workflow
already cross-references, `publish-agent-image.yml:39-40`) with the release
lane; append the DECISIONS.md ledger rows for the choices in Forks 1-4.

Ordering: T1 → T2 fold into one PR if small; T3 independent after T1; T4 lands
in the internal CD monorepo only after the RIG-2025 fixes are green on main AND
T1 has minted a Release at the advanced pin (the pin advance is T4 step 1); T5
with the record freeze.

## Tasks

- [ ] T1 — `release.yml` per-build prerelease lane (binaries + SHA256SUMS)
- [ ] T2 — release body generator (image digest pointer + nix-outputs.json)
- [ ] T3 — semver `v*` lane + `:vX.Y.Z` image tag via publish.sh
- [ ] T4 — internal config-publish cutover: go install → Release asset download
- [ ] T5 — docs/architecture update + DECISIONS.md ledger rows

## Resolved decisions and deferred questions

The three load-bearing questions were put to Matt and ruled (2026-08-22); the
rulings below are the frozen contract. OQ-4/5/6 stay deferred (non-load-bearing).

- **OQ-1 (load-bearing): binary set boundary. RESOLVED (Matt, 2026-08-22): DEFER
  the desktop lane.** v1 attaches `compass`, `compass-server`, `compass-runner`
  only. `compass-stack` / `compass-app` (the Wails desktop lane — platform-linked,
  `compass devenv.nix:189-190` notes the app links the system WebKit framework on
  macOS, so it does NOT cross-compile from an ubuntu runner) are out of scope
  here; their release-artifact distribution is decided in the native-packaging
  lane (`compass docs/designs/product/compass-native-packaging/design.md`, RIG-1687
  umbrella). The deferral is filed as **RIG-2477** (parented under RIG-1687) so it
  has a tracked home rather than living only as a design-record note.
- **OQ-2 (load-bearing): RIG-2103 disposition. RESOLVED (Matt, 2026-08-22):
  RETARGET.** RIG-2103 (formerly "compass CLI via a nix derivation") is retargeted
  onto T4's Release-asset download — one sourcing mechanism, checksum-verified,
  toolchain-free; a nix derivation adds hermeticity the CD job does not need.
  RIG-2103 now IS this record's T4 consumer-cutover task (its Linear scope was
  updated 2026-08-22 to match), gated on T1 minting a Release at the advanced pin.
- **OQ-3 (load-bearing): per-build lane trigger breadth. RESOLVED (Matt,
  2026-08-22): every qualifying push, binary-affecting scope.** The per-build
  prerelease lane triggers on every qualifying main push (cheap — binaries only,
  no nix image build), filtered by the binary-affecting paths (`go/**` + the
  toolchain pin), NOT unioned with the image lane's paths. An image-only sha
  (`agent-image/**`, `forks/**`, `bun.lock`) republishes the image but mints NO
  Release: its image identity stays GHCR-only (`:git-<sha12>` + digest) and is
  recorded in the *next* qualifying Release's body. Rationale: the Release lane
  versions downloadable binaries; the image already has an immutable per-sha GHCR
  identity, and unioning the filters would mint binary-identical Releases on
  image-only shas.
- **OQ-4 (non-load-bearing, deferred): closure attachment.** Whether an
  archival lane should ever attach full nix closures (`nix copy --to file://`)
  for offline reproduction. No consumer today; caches + pinned substituters
  (`ci.yml:165-168`) serve materialization. Revisit if an air-gapped consumer
  appears.
- **OQ-5 (non-load-bearing, deferred): prerelease retention.** `build-*`
  prereleases accumulate one per qualifying push. GitHub has no Release quota
  pressure; a later janitor (keep last N + all semver) can be added without
  contract change. Deferred.
- **OQ-6 (non-load-bearing, deferred): signed provenance.** GitHub artifact
  attestations (`attestations: write` + `gh attestation verify`) or cosign
  would bind assets to the building workflow identity; `SHA256SUMS` alone is
  transfer-integrity, not provenance (§Alternatives). No external consumer
  yet — revisit when one appears.
