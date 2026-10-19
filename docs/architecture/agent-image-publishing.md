# Agent image publishing

## Publishing the agent image

The `compass-agent` runtime image (the base every agent workstream runs in) is
published to GHCR by the unified `release.yml` lane, separate from the gate
above. For the full design see the frozen record
`RigelBuild/docs:docs/designs/compass/infra/release/compass-unified-release-lane/design.md` (the earlier
`RigelBuild/docs:docs/designs/compass/infra/ci/compass-agent-image-publish/design.md` records the
original image-publish rationale); the durable operational shape is here.

**Ref and tags.** The image is `ghcr.io/rigelbuild/compass-agent`. Every
closure-affecting push to main publishes two per-push tags, and a tagged release
adds a third:

- `:git-<sha12>` — the 12-hex short commit sha, **immutable**. This is the pin
  the native app bakes in and hands the runner via `--image` /
  `$COMPASS_AGENT_IMAGE`; it is the real consumption path. The publish refuses
  to overwrite an existing `:git-<sha>` whose content differs and re-inspects
  after each push to assert the digest landed, so the tag is immutable by
  enforcement, not convention.
- `:latest` — moving, documented **first-run fallback only**, never the default.
- `:vX.Y.Z` — the semver release tag, **immutable**, minted only when a
  `vX.Y.Z` release is cut. The release-gated `release-image` job digest-re-tags
  the already-published `:git-<sha>` image (no rebuild — same digest, a new
  pointer), so the released image is byte-identical to the per-push one.

The git-sha tag is pushed before `:latest`, so the immutable pin always exists
before the moving tag moves. The image is multi-arch: `publish-image-amd64` and
`publish-image-arm64` build natively in parallel, and `publish-image-manifest`
pushes one `linux/amd64` + `linux/arm64` index. The package is
**public** — compass is open-source, the image payload is public source, and it
carries no runtime secrets (those are runner-supplied per-exec) — so the
first-run pull needs no credential anywhere.

The weekly `agent-image-retention` workflow prunes old out-of-window `git-*`
versions while preserving release tags, off-main images, lock-pinned digests,
index children, and recent untagged publishes. Dispatch defaults to a dry-run
plan; GitHub keeps deleted versions restorable for 30 days.

**One derivation, two destinations.** The published `:git-<sha>` and the local
`dogfood:agent-image` load are copies of the *same* nix derivation — both flow
through the image's `container build agent`. They diverge only in the skopeo
destination (a registry ref versus `containers-storage:`), so what CI publishes
is byte-for-byte what a developer loads locally.

**The unified release lane.** Publishing lives in
`.github/workflows/release.yml`, **not** a job in `ci.yml` and **not** a
required check. Two jobs share it. The per-push `publish-image` job runs
main-only plus `workflow_dispatch`, closure-gated in-job (an in-job changed-path
check, not a trigger `paths:` filter — release-pr needs every commit),
with its own concurrency group set to `cancel-in-progress: false` (publishes
serialize rather than tear a tag pair mid-push) and a `packages: write` token
the gate job never gets; it publishes `:git-<sha>` + `:latest`. The
release-gated `release-image` job fires only when a `vX.Y.Z` release is cut and
digest-re-tags that image to `:vX.Y.Z`. It keeps the [CI](build-and-ci.md#ci) rule that no
workflow lists moon projects, because a list goes stale silently: release.yml
lists none (`agent-image/` is a standalone devenv, not a moon project). A
published tag is the source of truth; a missing per-push tag (paths filtered it
out, or a superseding push skipped it under the serialized concurrency group) is
**not** a failure — `workflow_dispatch` republishes any HEAD on demand.

The same workflow also runs `publish-runner-image`, `publish-server-image`,
`publish-guest-image`, and `publish-gateway-image`, plus the
`release-runner-image` and `release-server-image` re-tags. The gateway job builds and smoke-tests one local OCI
layout, then publishes its write-once `:git-<sha12>` tag and records the
`repo@sha256:…` identity to pin in the step summary.

**The release-please cadence.** `release.yml` also runs release-please, which
keeps a standing Release PR that batches conventional commits since the last
release. Merging that PR is the release: it cuts the `vX.Y.Z` git tag, updates
the CHANGELOG, and stamps the root `version.txt` to `X.Y.Z`. The tag then gates
the release jobs — the four-binary asset set is built stamped from `version.txt`
(so every released binary reports the same `X.Y.Z`), and `release-image` mints
the `:vX.Y.Z` image tag. Dev/bundle builds off `app-bundle/build.sh` read the
same `version.txt` base and append a `+g<shortsha>` suffix, so a local bundle
reports the release base it descends from. The standalone
`publish-agent-image.yml` predated this unification; it has been retired
(RIG-2994), its per-push publish duty relocated verbatim into `release.yml`'s
`publish-image` job.

**Smoke.** On a runner host, pull the immutable tag and drive the consumer seam:

```sh
podman pull ghcr.io/rigelbuild/compass-agent:git-<sha12>
compass-runner --image ghcr.io/rigelbuild/compass-agent:git-<sha12>
# then drive one provision
```

**One-time setup.** The first `GITHUB_TOKEN` push creates the package
private-by-default (and only if the org policy permits `GITHUB_TOKEN`-created
packages, else the push 403s and an owner must pre-create it). An owner sets the
package **public** once in its settings after that first push; the repo linkage
grants the workflow write access thereafter. Pre-creating the empty package
also settles the immutability guard's first-publish edge: the guard inspects
`:git-<sha>` *before* the creating copy and only an authoritative
`manifest unknown` frees the tag, so an owner-pre-created (hence
authenticatable) package guarantees the absent-tag inspect classifies cleanly
rather than on a not-yet-existent repository's error shape.

### Pre-merge build check

`agent-image/` is registered as the `compass-agent-image` moon project
(`.moon/workspace.yml`), so the CI gate builds the image on any PR that
affects its closure. Before this the image was outside moon and had zero
pre-merge coverage — an image-build break surfaced only post-merge in the
publish workflow, while a consumer waited on a tag.

The project's `build` task realises the image with the same pinned devenv
derivation the publish lane ships (`nix run "$src" -- container build agent`,
where `$src` is the devenv flakeref that `tools/toolchain/devenv-cli` resolves
from `agent-image/devenv.lock`), so a green build proves the exact artifact
that publishes still builds — both an eval-time break (a bun-pin drift against
the `agent-image/toolchain.nix` assert) and a realise-time break (an
`agent-image/entrypoint.nix` FOD-hash invalidation or a broken bundle), the
full class.

The Compass agent's compile script derives the SDK's lazy legacy-module registry from installed package exports and supplies it through a local Bun plugin. If the SDK adds a supported root option, Compass can remove this local plugin and use that option instead.

The build is heavy — the image closure is the dominant CI cost, the reason
the `moon` job's timeout is 90m — but it is not paid on every PR. `setup`
selects a PR's *affected* projects only, and the task's `inputs` scope it to the
image closure: the `agent-image/` tree, `packages/compass-agent/`, the root
`package.json` and `bun.lock`, and `tools/toolchain/versions/bun.nix`.
A PR that touches none of those never builds the image; every push to main runs
it unconditionally in the full sweep. Its `inputs` mirror the closure set the
`publish-image` job's in-job changed-path gate diffs over in `release.yml` (the
trigger carries no `paths:` filter — release-pr needs every commit, so the job
self-gates) — the reviewed source of truth for what changes the published
artifact — including the bun pin file, since the image now builds bun from that
pinned derivation, so a pin move there changes the output. As a `ci-group.nix`
project it runs in the `moon (nix)` leg behind `rollup`, so a build break
blocks merge, like every other build in CI.
