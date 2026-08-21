# Publish the compass-agent image to GHCR

> **Design record.** This designs the GHCR publish lane for the `compass-agent`
> runtime image; it targets the **`RigelBuild/compass`** repo — every
> `agent-image/*`, `.github/workflows/*`, `forks/*`, `go/cmd/*`, `ci.yml`,
> `devenv.nix`, `packages/compass-agent/*`, and `docs/architecture/*` citation
> below is a path in that repo at HEAD `b3fc25311`, not this one (line numbers
> drift as the code evolves; resolve them against that commit). It lives in the
> sealed design corpus (`docs/designs/platform/`) because that is where the wave's design
> records freeze; the `docs/designs/product/*` cross-references (e.g. DL-112)
> are paths in this (sealed) corpus.

Status: Draft
Tracking: SEA-1690 (blocks compass-native SEA-1683/T2, SEA-1685/T4, SEA-1687/T6)

## Problem / Intent

The `compass-agent` base image already builds and loads locally
(`dogfood:agent-image` → `containers-storage:compass-agent:latest`), but per
frozen decision DL-112 (`docs/designs/product/compass-native-app/design.md`
§OQ6) the native app does not bundle the agent image: `compass-stack` `podman
pull`s it from GHCR at first run. Nothing publishes the image today. This
record designs the publish lane — the GHCR side of that seam; the pull side is
compass-native's (SEA-1683/T2).

## Approach

### Decision: the name/tag contract (confirmed with compass-native)

Settled peer-to-peer with compass-native (the consumer) — recorded as a
decision, not an assumption:

- **Ref:** `ghcr.io/rigelbuild/compass-agent` — locked.
- **Tags, both published per main build:**
  - `:git-<short-sha>` (12-hex, `git rev-parse --short=12 HEAD`) — **immutable**.
    This is the pin compass-stack bakes into the native app binary and hands the
    runner via `--image` / `$COMPASS_AGENT_IMAGE`
    (`go/cmd/compass-runner/main.go:44-45` — "The container image every agent
    workstream runs. Defaults to $COMPASS_AGENT_IMAGE."; `:111-114` requires it).
    The publish step refuses to overwrite an existing `:git-<sha>` tag whose
    content differs (a config-digest compare, exact commands in T1), and
    re-inspects after every copy to assert the pushed digest — so immutability
    is enforced and verified, not assumed.
  - `:latest` — moving; documented **first-run fallback only**, never the
    default consumption path.
- **Forward note (non-load-bearing):** GA (SEA-1687) will likely add a
  release-version tag (e.g. `:v1.2.3`) alongside `:git-<sha>`. The publish
  script below takes its tag list as arguments, so that is a one-line later
  add — no scheme change.
- **Visibility:** the package is **public** (decided — see the visibility
  Decision below). The lane works either way; public means no pull credential
  anywhere.

### Decision: platform contract — `linux/amd64` single-arch (dogfood milestone)

A settled fact, not an open question: the consumer's arch is **frozen** in
compass-native's merged record (PR #1073,
`docs/designs/product/compass-native-app/design.md`) to Linux x86_64 for the
dogfood milestone — non-Linux runner support and
macOS packaging are deferred there to a GA follow-up (`:522-524` "reproducible
build of the app bundle for Linux (the dev/dogfood target; macOS packaging
tracked as follow-up per A5)"; `:246-248` "non-Linux runner support … deferred
to follow-up"; `:346-349` runner is Linux-only with an embedded-mode
preflight-and-refuse on an unsupported host; `:486` dogfood gate = "e2e smoke on
the Linux dev box", which is x86_64). So publish and consumer agree on
`linux/amd64`, and the single-arch two-`skopeo copy` mechanism below is correct
**by design, not by omission**.

- **Forward add (non-load-bearing), tied to GA native-packaging (compass-native
  T5/T6):** macOS/`aarch64` support is both a native-packaging item *and* a
  multi-arch manifest-list image here. The per-platform entrypoint FOD hash
  (`agent-image/entrypoint.nix:96-116`) and the manifest-list push (which the
  two-copy path does not produce — needing per-arch builds on matching runners
  plus a `manifest create`/`push`) enter scope in *that* record, not this one.

### Decision: build+push mechanism — build the nix2container spec once, push each tag with the fork's skopeo

The image is a devenv/nix2container artifact, not a docker build. The devenv
container module builds a nix2container image spec and copies it with a
patched skopeo that understands the `nix:` transport
(`forks/devenv/src/modules/containers.nix:307`):

```text
${nix2container.skopeo-nix2container}/bin/skopeo --insecure-policy copy "nix:$container" "$dest"
```

with `dest="${registry}${cfg.name}:${cfg.version}"` (`containers.nix:295`) —
the tag is pinned to `cfg.version`, which defaults to `"latest"`
(`containers.nix:324-328`) and is not overridable from the CLI: `devenv
container copy` exposes only `--registry` and `--copy-args`
(`forks/devenv/devenv/src/cli.rs:1104-1113`). So the stock copy path can push
`:latest` but can never mint `:git-<sha>`.

**Chosen mechanism:**

1. Build the spec exactly as dogfood does: `nix run path:../forks/devenv#devenv
   -- container build agent`, cwd `agent-image/`. `container build` prints the
   image-spec store path to stdout (`forks/devenv/devenv/src/main.rs:910-912`
   emits it as `CommandResult::Print`, a bare `print!` after UI cleanup,
   `main.rs:830-832`; `container copy` calls the same `container_build`
   internally, `forks/devenv/devenv/src/devenv/container.rs:55`), so this is
   byte-for-byte the derivation the dogfood task loads into `containers-storage:`
   — the reproducibility constraint holds by construction: one derivation, two
   copy destinations. Capture hygiene: nix/devenv tracing goes to stderr, but
   the script takes the last stdout line and asserts it matches `^/nix/store/`
   before use, so a future fork bump that adds a stdout line cannot silently
   feed skopeo a non-path.
2. Push each tag straight from the spec with the fork's patched skopeo
   (`forks/nix2container/flake.nix:31` exposes `skopeo-nix2container`;
   `default.nix:22` builds it from `pkgs.skopeo` with the `nix:` transport
   patch — the same pattern nix2container's own `copyToRegistry` helper uses,
   `default.nix:78-81`):

   ```text
   skopeo --insecure-policy copy nix:$SPEC docker://ghcr.io/rigelbuild/compass-agent:git-<sha>
   skopeo --insecure-policy copy nix:$SPEC docker://ghcr.io/rigelbuild/compass-agent:latest
   ```

   The second copy re-uploads nothing: skopeo skips blobs the registry already
   has, so `:latest` costs one manifest write.

**Alternatives weighed:**

- *(a) `devenv container copy agent --registry docker://ghcr.io/rigelbuild/`
  then retag* — reuses the highest-level CLI, but can only produce `:latest`
  (tag fixed at `cfg.version`), forcing a registry→registry retag copy for the
  immutable tag and pushing the moving tag *first* — the wrong order (the pin
  should exist before `:latest` moves). Rejected: more moving parts for less
  control.
- *(b) wire nix2container's `copyToRegistry` as a nix attr* — the devenv module
  does not expose the raw image derivation as a build target (`derivation` is
  an `internal = true` option, `containers.nix:523-527`), so this means new
  plumbing through the vendored fork for something two skopeo invocations do.
  Rejected: fork surface area for no gain.
- *(c) `podman push` from `containers-storage:` after a dogfood-style load* —
  round-trips through the local store and re-computes layers on push; loses the
  direct spec→registry path and adds a store dependency to CI. Rejected.

### Decision: CI placement — a separate least-privilege workflow, a principled exception to ONE-JOB

New workflow `.github/workflows/publish-agent-image.yml`, **not** a step in the
existing `CI` job.

The ONE-JOB doctrine (`.github/workflows/ci.yml:4-23`) exists to prevent a
second source of truth for *what the gate covers*: "it can only be built by
enumerating projects (or tasks) in YAML, and a project list in a workflow is a
second source of truth for something .moon/workspace.yml already owns." The
publish lane is not the gate and enumerates no moon projects — `agent-image/`
is deliberately not a moon project (`.moon/workspace.yml` has no entry for it;
it is a standalone devenv) — so a separate workflow re-creates none of the
silent-staleness failure the doctrine targets. What it *does* buy:

- **Least privilege.** A GHCR push needs `packages: write`; the gate job runs
  `contents: read` (`ci.yml:76-77`, workflow-level). GitHub permissions *can*
  be scoped per-job, so a second job in `ci.yml` with its own `packages: write`
  would not widen the gate job's token — but a separate workflow is the simpler
  boundary: it gets `contents: read` + `packages: write` and nothing else, and
  PR events never reach it at all.
- **Paths-scoping.** `ci.yml`'s `on:` has no per-job path filter; scoping
  publish to closure-affecting pushes inside `ci.yml` would need a
  `dorny/paths-filter` step, where a separate workflow gets native
  `on.push.paths` (next section).
- **Concurrency.** The gate's concurrency group cancels in-progress runs
  (`ci.yml:74-76`, `cancel-in-progress: true`) — a superseding push would kill
  an in-flight publish mid-tag-pair. The publish needs its own
  `cancel-in-progress: false` group, which a separate workflow gives cleanly.
- **Off the hot path, not a required check.** The image closure is the heavy
  nix build that motivates CI's 90m timeout (`ci.yml:93-95`); the dogfood task
  is opt-in for the same reason (`devenv.nix:340-348`). A separate workflow
  keeps PR latency untouched, and a publish flake never reds the required merge
  gate.
- **Failure ownership.** `agent-image/` is not a moon project, so the gate
  never builds it — the image has *zero* pre-merge coverage, and the publish is
  not required. A red publish (a `.prototools`/nixpkgs bun-drift assert,
  `agent-image/toolchain.nix:50-56`; an FOD-hash invalidation,
  `entrypoint.nix:116`) would otherwise sit unnoticed while a consumer waits on
  a tag. Publish failures are owned by compass-runner (this record's owner);
  the main-branch Actions failure notification is the surfacing mechanism for a
  push failure. The image BUILD is now gated pre-merge by the
  `compass-agent-image` moon project (the deferred-hardening item below,
  implemented), so a build break is caught on the PR rather than here.

Triggers: `push: branches: [main]` with a `paths:` filter (next section), plus
`workflow_dispatch` as the manual/backstop lane. No PR trigger — no secret or
token exposure to fork PRs, no per-PR heavy build. No tag/release trigger yet;
that arrives with the GA release-version tag (forward note above).

*Alternative weighed:* fold into the `CI` job as a main-only conditional step.
Keeps one workflow file, but widens the gate token (above), couples publish
latency/failures to the required check, and buys nothing — the publish shares
no setup with the gate beyond nix installation. Rejected.

### Decision: publish only when the image closure changes, with a manual backstop

`paths:` filter on the workflow's `push` trigger, approximating the image's
nix closure:

```text
agent-image/**
packages/compass-agent/**
forks/devenv/**
forks/nix2container/**
package.json
bun.lock
.github/workflows/publish-agent-image.yml
```

(`agent-image/entrypoint.nix` bundles `packages/compass-agent` and pins the
whole workspace dependency tree to the root `bun.lock` via a fixed-output
derivation whose src reads every workspace-member manifest —
`(builtins.fromJSON (readFile ../package.json)).workspaces.packages` expanded
with `readDir` over `packages/*`/`apps/*`/`tools/*`, `entrypoint.nix:48-65`,
realised at `:140-148`; the build itself runs through `forks/devenv` and its
nix2container input. `.prototools` is deliberately *excluded* — a bun-pin move
there can only fail the `toolchain.nix:50-56` assert, never silently change the
output, so it needs no publish trigger. The workflow file itself is in the
filter so a fix to the publish lane republishes.)

- The build is the expensive part (the full agent toolchain closure), not the
  transfer — GHCR layer dedup makes an identical push cheap, but the nix build
  that precedes it is exactly the cost the repo keeps off every hot path.
  Publishing on every main push would pay it on every docs/Go/UI commit for an
  identical artifact.
- The cost of filtering: a path the filter misses (e.g. a future input outside
  these globs) skips a publish, and `:git-<sha>` tags exist only for shas where
  publish ran. The same is true of concurrency supersession — GitHub keeps at
  most one pending run per group, so a burst of pushes A,B,C publishes A and C
  and silently skips B's tag even though its paths matched. Both gaps are
  acceptable and mean the same thing: **a published tag is the source of
  truth, a missing one is not a failure.** compass-stack pins a *published*
  sha (it selects the pin at native-app build time from published tags, so
  gaps are fine), and `workflow_dispatch` republishes any HEAD on demand. The
  filter list lives beside the workflow with a comment tying it to the closure
  inputs, so extending the closure prompts extending the filter in review.

*Alternative weighed:* publish on every main push (simplest, no filter to go
stale). Rejected for CI spend: the heavy nix build per commit is the exact cost
`ci.yml`'s affected gating and the opt-in dogfood task exist to avoid.

### Decision: push auth — the workflow's own `GITHUB_TOKEN`

`skopeo login ghcr.io -u ${{ github.actor }} --password-stdin` fed from
`secrets.GITHUB_TOKEN`, with `permissions: packages: write` on the workflow.
Scoped to this repo's GHCR namespace, auto-rotated, nothing to provision or
leak. *Alternative:* a PAT/deploy token — needed only for cross-repo or
cross-org pushes, which this is not; a standing secret to rotate for zero
benefit. Rejected.

**Auth-file pin (load-bearing across the two `nix run` invocations):** `skopeo
login` and the later `skopeo copy` run as separate `nix run` processes and must
resolve the *same* credentials file. The default location is
`$XDG_RUNTIME_DIR/containers/auth.json`, whose presence on GitHub-hosted
runners is environment-dependent — a mismatch greens the login step and 401s
the copy. The workflow therefore exports an explicit `REGISTRY_AUTH_FILE`
(e.g. `$RUNNER_TEMP/ghcr-auth.json`) that both invocations honor; `publish.sh`
passes `--authfile "$REGISTRY_AUTH_FILE"` to every skopeo call as a belt-and-
braces guard.

First-publish caveat (operational, one-time): the first `GITHUB_TOKEN` push
creates the `compass-agent` package linked to this repo, **private by
default**, and only if the org's package-creation policy permits
`GITHUB_TOKEN`-created packages (an org setting; if it forbids them, the first
run 403s and an owner must pre-create the package or relax the policy). Whoever
executes the plan must confirm that precondition, set the package **public**
(per the visibility Decision below) in the package settings once, and confirm
the repo-linkage grants the workflow write access thereafter.

### Decision: package visibility — public (Matt's ruling)

The GHCR package is **public**. Matt ruled it directly: compass is
open-source, so the image's payload — the toolchain plus the bundled
first-party agent entrypoint (readable, unminified `bun build` JS of
`packages/compass-agent` and its import graph) — is source that is public by
the project's own nature, not a leak. The image carries no runtime secrets
(those are runner-supplied per-exec, `agent-image/devenv.nix:50-53`). Public is
also what makes DL-112's no-credential first-run `podman pull` work with
nothing to provision.

Consequences folded through this record: the pull side needs no credential (the
DL-109 keychain flow is a private-visibility contingency only, kept in the note
below for the record); the one-time package-visibility setting after first push
is "set public"; and an unauthenticated `skopeo inspect` becomes a meaningful
acceptance signal (a T3 follow-up — it exercises the actual anonymous consumer
path, and is not folded into T3's write-token-scoped automated coverage). A public
package must not be flipped private later without a migration — that would break
every installed consumer's pull.

### Note: pull-side auth (compass-native's card, recorded for the dependency)

On the `linux/amd64` dogfood path: if the package is public, `compass-stack`'s
first-run `podman pull` needs no credential. If private, compass-native builds a
keychain-first pull-cred flow into compass-stack (per their DL-109 posture — on
dogfood's Linux host that is a Secret Service keychain with a `0600` fallback;
cheap on their side, by their own assessment). With the package public (decided
above) this pull-cred flow is **not needed for dogfood** — it stays a private-
visibility contingency only, recorded so the dependency is explicit if
visibility ever changes.

## Global Constraints

- Conventional Commits; this design PR is `docs(platform): …` with the
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` trailer.
- No AI-product names; no planning metadata in source. SEA-#### appears only
  as tracking refs in this record.
- Comments and docs explain non-obvious WHY (compass `AGENTS.md`).
- The image build stays OFF the hot `up`/PR path: publish is main-only +
  `workflow_dispatch`, mirroring the opt-in `dogfood:agent-image` posture
  (`devenv.nix:345-348` — "NOT wired `after` into up — the image closure is
  large").
- Reproducibility: the published `:git-<sha>` and the local dogfood load are
  copies of the SAME nix derivation — both flow through
  `container_build agent` (`forks/devenv/devenv/src/devenv/container.rs:55`)
  and diverge only in skopeo destination.
- `:git-<sha>` tags are immutable: the publish script refuses to overwrite a
  tag whose remote config digest differs from the local spec's, and a
  deliberate re-publish of the same sha is a no-op success only when the
  digests match. A registry error that is *not* an unambiguous manifest-unknown
  aborts (never falls through to an overwrite).
- ONE-JOB doctrine (`ci.yml:4-23`) is respected: the gate workflow is
  untouched; the publish workflow enumerates no moon projects and is not a
  required check.

## Plan

### T1 — Publish script

Add `agent-image/publish.sh`: build the spec, then push the tag list. Runnable
locally (with a PAT-backed `skopeo login`) and from CI identically.

- Build: `nix run path:../forks/devenv#devenv -- container build agent`
  executed with cwd `agent-image/` (the same fork-pinned invocation shape as
  `dogfood:agent-image`, `devenv.nix:349-354`); capture the printed image-spec
  store path.
- Skopeo: `nix run path:../forks/nix2container#skopeo-nix2container --`
  (exposed at `forks/nix2container/flake.nix:31`; `pkgs.skopeo`'s
  `meta.mainProgram` survives the `overrideAttrs` at `default.nix:22`, so
  `nix run` resolves `bin/skopeo`).
- For each requested tag, in order (`git-<sha>` before `latest`, so the
  immutable pin lands before the moving tag), a `git-*` tag runs the
  immutability guard first:
  1. **Read the remote config digest.** `skopeo inspect --raw
     docker://…:<tag> | jq -r .config.digest`. Plain `skopeo inspect`'s
     `.Digest` is the *manifest* digest, which skopeo may re-serialize on copy;
     `--raw` + `.config.digest` is the rootfs-pinning identity the fork itself
     compares (`GetConfigDigest`, `forks/nix2container/nix/image.go:40-47`) and
     survives a copy verbatim.
  2. **Error taxonomy — never fall open.** If the inspect fails, distinguish
     *manifest-unknown* (tag genuinely absent: GHCR returns
     `manifest unknown` / HTTP 404 — parse stderr for it) from any other
     failure (auth 401/403, network 5xx, rate-limit). Manifest-unknown → the
     tag is free, proceed to copy. Any other error → **abort non-zero**; a
     transient registry hiccup must never be read as "tag absent, safe to
     overwrite".
  3. **Compare.** Read the local spec's config digest the same way
     (`skopeo inspect --raw nix:$SPEC | jq -r .config.digest`). Equal →
     idempotent re-run, skip the copy (no-op success). Differ → fail with a
     "tag exists — immutable" error.
- Copy: `skopeo --insecure-policy copy nix:$SPEC
  docker://ghcr.io/rigelbuild/compass-agent:<tag>` (`--insecure-policy`
  matches the module's own copy invocation, `containers.nix:307`).
- **Post-copy assert.** After each copy, re-inspect the pushed tag's config
  digest and assert it equals the local spec's — upgrading the guarantee from
  "skopeo exited 0" to "the artifact I built is the artifact at that tag", and
  closing the check-then-copy TOCTOU window's *damage* (a racing overwrite is
  detected, not silently accepted).
- Auth: `--authfile "$REGISTRY_AUTH_FILE"` on every skopeo call (never an
  ambient-only login — see the auth-file pin above).
- **Verify the guard's linchpin first.** The whole guard (local-digest read,
  compare, post-copy re-inspect) rests on `skopeo inspect --raw nix:$SPEC`
  working as a standalone inspect through the fork's `nix:` transport — the one
  claim resting on a build-time-fetched patch (`forks/nix2container/default.nix`
  pulls the transport from `github.com/nlewo/container-libs`) rather than an
  in-tree file. The first implementation spike must confirm `skopeo inspect
  --raw nix:$SPEC | jq -r .config.digest` succeeds against the patched skopeo
  before the guard is built on it, so a broken assumption fails at T1, not at
  first publish. (Strongly expected to hold — the same `nix:` ImageSource
  already backs `skopeo copy`, `containers.nix:307` — but out-of-tree, so
  verify.)

Interfaces:

- Consumes: cwd `agent-image/`; `forks/devenv` + `forks/nix2container` flakes;
  git HEAD for the sha tag; an existing `skopeo login ghcr.io` session.
- Produces: pushed
  `docker://ghcr.io/rigelbuild/compass-agent:git-<sha12>` and
  `docker://ghcr.io/rigelbuild/compass-agent:latest`; exits non-zero on
  immutability violation or copy failure.
- CLI: `./publish.sh [tag …]` (no args = the default two-tag set; the GA
  release tag later becomes `./publish.sh git-<sha> v<semver> latest`).

### T2 — Publish workflow

Add `.github/workflows/publish-agent-image.yml`:

- `on: push: branches: [main]` with the `paths:` filter from the Approach
  (globs listed there, with a header comment tying each glob to the closure
  input it tracks), plus `workflow_dispatch`.
- **`workflow_dispatch` ref guard.** `workflow_dispatch` runs on any branch, so
  the publish job is guarded `if: github.ref == 'refs/heads/main'` — a dispatch
  from a feature branch must never mint a `:git-<sha>` for unmerged code nor
  move `:latest` off main. (Main pushes satisfy the guard trivially.)
- `permissions: contents: read, packages: write` (workflow-level; nothing
  else).
- `concurrency: group: publish-agent-image, cancel-in-progress: false` —
  publishes serialize rather than cancel, so an in-flight `:latest` move is
  never half-superseded (a superseded push skips its tag, per the Approach —
  never a torn tag pair).
- Steps: checkout (default depth — the script needs only HEAD);
  `cachix/install-nix-action` pinned to the same sha + `extra_nix_config`
  block as `ci.yml:227-245` (same substituters, same reviewed-file trust
  rationale — never `accept-flake-config`); `skopeo login ghcr.io -u
  ${{ github.actor }} --password-stdin <<< "$GITHUB_TOKEN"` via the fork's
  skopeo; run `agent-image/publish.sh`.
- `timeout-minutes: 90` (the image closure is the same heavy build that sizes
  the CI job's timeout, `ci.yml:93-95`).

Interfaces:

- Consumes: `agent-image/publish.sh` (T1); `secrets.GITHUB_TOKEN`;
  `github.actor`.
- Produces: the two pushed refs above on every closure-affecting main push;
  a manually dispatchable republish of HEAD.
- Not a required check; never triggered by `pull_request`.

### T3 — Verification

Two layers, one automated + one documented smoke:

- In-workflow (end of T2's job), all with `--authfile "$REGISTRY_AUTH_FILE"`:
  `skopeo inspect docker://…:git-<sha12>` must succeed — proves the artifact is
  resolvable from GHCR, not merely that the copy exited 0. Assert `.Architecture`
  and `.Os` match the build platform (the cheapest tripwire for a platform-
  contract regression, see the `linux/amd64` platform Decision above), and
  assert `:latest`'s config digest equals `:git-<sha12>`'s (one inspect each —
  proves the two-copy pair
  landed coherently). Automated coverage ends here: at "the manifest is
  resolvable with the workflow's own write-scoped token"; it does not exercise
  the visibility-dependent anonymous pull (public, so anonymous pull is the
  contract — an unauthenticated `skopeo inspect` is the truest check, notable
  as a T3 follow-up) nor a real pull-and-run.
- Documented smoke (in the workflow header comment and the T4 doc fold): on a
  runner host, `podman pull ghcr.io/rigelbuild/compass-agent:git-<sha12>`
  then start `compass-runner --image
  ghcr.io/rigelbuild/compass-agent:git-<sha12>` and drive one provision —
  exercising the exact consumer seam
  (`go/cmd/compass-runner/main.go:44-45,111-114`). This is the acceptance
  check compass-native T2 repeats from the pull side.

Interfaces:

- Consumes: the pushed refs from T2; a rootless-podman host with the runner
  built.
- Produces: a green publish run implies GHCR-resolvability; the documented
  smoke procedure lives in the workflow header + `build-and-ci.md`.

### T4 — Doc fold

After this record freezes and T1–T3 land: add a short "Publishing the agent
image" section to `docs/architecture/build-and-ci.md` (which already owns the
CI architecture narrative) covering the ref/tag contract, the one-derivation
reproducibility guarantee, the separate-workflow rationale (least privilege,
not a gate), and the smoke procedure. This record stays the design of record;
`build-and-ci.md` carries the durable operational description.

Interfaces:

- Consumes: the merged T1–T3 artifacts; this record.
- Produces: an updated `docs/architecture/build-and-ci.md`; no code changes.

## Tasks

- [ ] T1: `agent-image/publish.sh` — spec build + immutable-guarded two-tag
      skopeo push.
- [ ] T2: `.github/workflows/publish-agent-image.yml` — main-only,
      path-filtered, `workflow_dispatch`, least-privilege `packages: write`,
      `GITHUB_TOKEN` auth.
- [ ] T3: in-workflow `skopeo inspect` verification + documented
      `podman pull` / `--image` smoke.
- [ ] Platform contract: `linux/amd64` single-arch (settled per compass-native's
      frozen record #1073); T3 asserts `.Architecture`/`.Os` as the tripwire.
      macOS/`aarch64` multi-arch is a forward add in the GA native-packaging
      record, not here.
- [ ] T4: fold the durable publish description into
      `docs/architecture/build-and-ci.md`.
- [ ] One-time (whoever lands T2): set the GHCR package **public** after the
      first push (Matt's ruling — compass is OSS).
- [x] Deferred hardening (T4-adjacent): gate the image build pre-merge so an
      image-build break is caught at PR time instead of in the post-merge
      publish. Implemented as the `compass-agent-image` moon project
      (`agent-image/moon.yml`), which realises the full image in the CI gate on
      any PR affecting the image closure. An earlier eval-only variant (`nix
      eval` the drv without realising) was superseded by Matt's ruling to build
      the full image: eval-only caught only the eval-time bun-drift assert,
      whereas the moon build also catches realise-time breaks (FOD-hash
      invalidation, a broken bundle), and reuses the same nix-in-gate posture
      the vendored forks already establish.

## Open Questions

- **[Resolved] Record placement.** This record lives in the sealed design
  corpus (`docs/designs/platform/`), the wave's canonical home for frozen
  design records, beside the other `compass-*` records. T4 folds the durable
  operational content into the compass repo's `docs/architecture/build-and-ci.md`
  once implemented; this record stays the design of record in sealed.
- **[Non-load-bearing] GA release tag.** SEA-1687 will likely want
  `:v<semver>` alongside `:git-<sha>`; T1's tag-list CLI makes that a
  no-redesign later add. Deferred to GA planning.
- **[Non-load-bearing] `:git-<sha>` tag retention.** Immutable per-sha tags
  accumulate on GHCR unbounded. No retention policy now (cost is negligible at
  this volume); a later cleanup (keep last N, or prune unreferenced shas) is a
  no-redesign add. Noted so it is a deliberate deferral, not an oversight.
